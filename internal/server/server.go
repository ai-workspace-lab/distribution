package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ai-workspace-lab/distribution/internal/config"
	"github.com/ai-workspace-lab/distribution/internal/storage"
)

// Handler serves all 8 distribution ecosystems with CDN-friendly cache semantics.
// It is 100% pure Go net/http and runs identically in `distd serve` and Cloudflare Workers (WASM).
type Handler struct {
	cfg    *config.Config
	store  storage.Backend
	client *http.Client
}

func NewHandler(cfg *config.Config, store storage.Backend, client *http.Client) *Handler {
	if cfg == nil {
		cfg = config.DefaultConfig()
	}
	if store == nil {
		store = storage.NewMemoryBackend("")
	}
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	return &Handler{
		cfg:    cfg,
		store:  store,
		client: client,
	}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Standard CORS headers for browser & package-client friendliness
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, HEAD, POST, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Accept, Range, If-None-Match")
	w.Header().Set("X-Distribution-Mirror", h.mirrorName())

	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	p := r.URL.Path
	switch {
	case p == "/healthz":
		h.writeJSON(w, http.StatusOK, map[string]any{
			"status":  "ok",
			"mirror":  h.mirrorName(),
			"storage": h.store.Name(),
			"engine":  "distribution-v2-go",
		}, "no-cache")
		return
	case p == "/repo/_health/probe.json":
		h.serveStaticWithFallback(w, r, "repo/_health/probe.json", func() (any, string) {
			return map[string]any{
				"status":        "ok",
				"mirror":        h.mirrorName(),
				"repoHost":      strings.TrimPrefix(strings.TrimPrefix(h.publicBaseURL(r), "https://"), "http://"),
				"dlHost":        strings.TrimPrefix(strings.TrimPrefix(h.dlBaseURL(), "https://"), "http://"),
				"productsCount": len(h.cfg.Products),
			}, "public, max-age=30"
		})
		return
	case p == "/" || p == "/index.html":
		h.serveObject(w, r, "index.html", false)
		return
	case p == "/v2" || p == "/v2/" || strings.HasPrefix(p, "/v2/"):
		h.handleOCI(w, r)
		return
	case p == "/npm" || strings.HasPrefix(p, "/npm/"):
		h.handleNPM(w, r)
		return
	case p == "/helm" || strings.HasPrefix(p, "/helm/"):
		h.handleHelm(w, r)
		return
	case p == "/winget/api" || strings.HasPrefix(p, "/winget/api/"):
		h.handleWinGetAPI(w, r)
		return
	case strings.HasPrefix(p, "/git/") || strings.HasPrefix(p, "/github/") || strings.HasPrefix(p, "/raw/"):
		h.handleGitHub(w, r)
		return
	case strings.HasPrefix(p, "/files/"):
		key := strings.TrimPrefix(p, "/")
		h.serveObject(w, r, key, h.cfg.CDNRedirect && isImmutablePath(p))
		return
	case strings.HasPrefix(p, "/releases/"):
		key := strings.TrimPrefix(p, "/")
		h.serveObject(w, r, key, h.cfg.CDNRedirect && isImmutablePath(p))
		return
	default:
		// Serves /apt/*, /rpm/*, /homebrew/*, /scoop/*, /winget/manifests/*, /install.sh, /install.ps1, /repo-index.json
		key := strings.TrimPrefix(p, "/")
		h.serveObject(w, r, key, h.cfg.CDNRedirect && isImmutablePath(p))
		return
	}
}

func (h *Handler) mirrorName() string {
	if h.cfg.MirrorName != "" {
		return h.cfg.MirrorName
	}
	return "global"
}

func (h *Handler) publicBaseURL(r *http.Request) string {
	if h.cfg.PublicURL != "" {
		return strings.TrimRight(h.cfg.PublicURL, "/")
	}
	scheme := "https"
	if r != nil && r.TLS == nil && (strings.HasPrefix(r.Host, "localhost") || strings.HasPrefix(r.Host, "127.0.0.1")) {
		scheme = "http"
	}
	if r != nil && r.Host != "" {
		return scheme + "://" + r.Host
	}
	return "https://repo.svc.plus"
}

func (h *Handler) dlBaseURL() string {
	if h.cfg.DLURL != "" {
		return strings.TrimRight(h.cfg.DLURL, "/")
	}
	if h.mirrorName() == "cn" && h.cfg.Mirrors.CNDL != "" {
		return strings.TrimRight(h.cfg.Mirrors.CNDL, "/")
	}
	if h.cfg.Mirrors.DL != "" {
		return strings.TrimRight(h.cfg.Mirrors.DL, "/")
	}
	return "https://dl.svc.plus"
}

func (h *Handler) serveStaticWithFallback(w http.ResponseWriter, r *http.Request, key string, fallback func() (any, string)) {
	obj, err := h.store.Get(r.Context(), key)
	if err == nil {
		defer obj.Body.Close()
		h.writeObjectResponse(w, r, key, obj)
		return
	}
	payload, cc := fallback()
	h.writeJSON(w, http.StatusOK, payload, cc)
}

func (h *Handler) serveObject(w http.ResponseWriter, r *http.Request, key string, allowRedirect bool) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if allowRedirect {
		if pub := h.store.PublicURL(key); pub != "" && !strings.HasPrefix(pub, h.publicBaseURL(r)+"/") {
			if _, err := h.store.Head(r.Context(), key); err == nil {
				w.Header().Set("Cache-Control", CacheControlForPath("/"+key))
				http.Redirect(w, r, pub, http.StatusFound)
				return
			}
		}
	}

	if r.Method == http.MethodHead {
		meta, err := h.store.Head(r.Context(), key)
		if err != nil {
			if errors.Is(err, storage.ErrNotFound) {
				http.NotFound(w, r)
				return
			}
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		h.applyMetaHeaders(w, key, meta)
		if checkNotModified(w, r, meta.ETag) {
			return
		}
		w.WriteHeader(http.StatusOK)
		return
	}

	obj, err := h.store.Get(r.Context(), key)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer obj.Body.Close()
	h.writeObjectResponse(w, r, key, obj)
}

func (h *Handler) writeObjectResponse(w http.ResponseWriter, r *http.Request, key string, obj *storage.Object) {
	h.applyMetaHeaders(w, key, obj.Meta)
	if checkNotModified(w, r, obj.Meta.ETag) {
		return
	}
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodGet {
		_, _ = io.Copy(w, obj.Body)
	}
}

func (h *Handler) applyMetaHeaders(w http.ResponseWriter, key string, meta storage.ObjectMeta) {
	if meta.ContentType != "" {
		w.Header().Set("Content-Type", meta.ContentType)
	}
	if meta.Size > 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(meta.Size, 10))
	}
	if meta.ETag != "" {
		w.Header().Set("ETag", meta.ETag)
	}
	if !meta.LastModified.IsZero() {
		w.Header().Set("Last-Modified", meta.LastModified.UTC().Format(http.TimeFormat))
	}
	cc := meta.CacheControl
	if cc == "" {
		cc = CacheControlForPath("/" + key)
	}
	w.Header().Set("Cache-Control", cc)
	w.Header().Set("Accept-Ranges", "bytes")
}

func checkNotModified(w http.ResponseWriter, r *http.Request, etag string) bool {
	if etag == "" {
		return false
	}
	if inm := r.Header.Get("If-None-Match"); inm != "" && strings.TrimSpace(inm) == etag {
		w.WriteHeader(http.StatusNotModified)
		return true
	}
	return false
}

func (h *Handler) writeJSON(w http.ResponseWriter, status int, payload any, cacheControl string) {
	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	data = append(data, '\n')
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if cacheControl != "" {
		w.Header().Set("Cache-Control", cacheControl)
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.WriteHeader(status)
	_, _ = w.Write(data)
}

// CacheControlForPath returns CDN-friendly Cache-Control directives.
func CacheControlForPath(path string) string {
	if isImmutablePath(path) {
		return "public, max-age=31536000, immutable"
	}
	return "public, max-age=60, stale-while-revalidate=300"
}

func isImmutablePath(path string) bool {
	low := strings.ToLower(path)
	if strings.Contains(low, "/blobs/sha256:") || strings.Contains(low, "/manifests/sha256:") {
		return true
	}
	if strings.Contains(low, "/apt/pool/") || strings.Contains(low, "/rpm/packages/") {
		return true
	}
	if strings.Contains(low, "/-/") && strings.HasSuffix(low, ".tgz") {
		return true
	}
	if strings.HasPrefix(low, "/helm/charts/") && strings.HasSuffix(low, ".tgz") {
		return true
	}
	for _, ext := range []string{".deb", ".rpm", ".dmg", ".msi", ".exe", ".apk", ".aab", ".ipa", ".appimage", ".tar.gz", ".zip", ".tgz", ".pack", ".idx"} {
		if strings.HasSuffix(low, ext) {
			return true
		}
	}
	return false
}
