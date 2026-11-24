package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/ai-workspace-lab/distribution/internal/storage"
)

// handleNPM implements a CDN-friendly npm read registry with optional upstream pull-through caching.
// Supports:
//   - GET /npm/<pkg> (or scoped /npm/@scope/pkg and URL-encoded /npm/@scope%2Fpkg)
//   - GET /npm/<pkg>/-/<tarball>.tgz
func (h *Handler) handleNPM(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	raw := strings.TrimPrefix(r.URL.EscapedPath(), "/npm")
	raw = strings.TrimPrefix(raw, "/")
	if raw == "" {
		h.writeJSON(w, http.StatusOK, map[string]any{
			"registry":    "svc.plus-npm",
			"mirror":      h.mirrorName(),
			"upstream":    h.cfg.Upstreams.NPM.UpstreamRegistry,
			"pullThrough": h.cfg.Upstreams.NPM.PullThrough,
		}, "public, max-age=60")
		return
	}

	decoded, err := url.PathUnescape(raw)
	if err != nil {
		decoded = raw
	}

	// Tarball request: <pkg>/-/<file>.tgz
	if strings.Contains(decoded, "/-/") {
		storeKey := "npm/tarballs/" + storage.CleanKey(decoded)
		if obj, err := h.store.Get(r.Context(), storeKey); err == nil {
			defer obj.Body.Close()
			if h.cfg.CDNRedirect {
				if pub := h.store.PublicURL(storeKey); pub != "" && !strings.HasPrefix(pub, h.publicBaseURL(r)+"/") {
					w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
					http.Redirect(w, r, pub, http.StatusFound)
					return
				}
			}
			h.writeObjectResponse(w, r, storeKey, obj)
			return
		} else if !errors.Is(err, storage.ErrNotFound) {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}

		if !h.cfg.Upstreams.NPM.PullThrough {
			http.NotFound(w, r)
			return
		}

		upstreamURL := strings.TrimRight(h.cfg.Upstreams.NPM.UpstreamRegistry, "/") + "/" + raw
		req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, upstreamURL, nil)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		resp, err := h.client.Do(req)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			http.Error(w, "upstream npm tarball error", resp.StatusCode)
			return
		}
		data, err := io.ReadAll(resp.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		_ = h.store.Put(r.Context(), storeKey, bytes.NewReader(data), int64(len(data)), storage.PutOptions{
			ContentType:  "application/octet-stream",
			CacheControl: "public, max-age=31536000, immutable",
		})
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		w.WriteHeader(http.StatusOK)
		if r.Method == http.MethodGet {
			_, _ = w.Write(data)
		}
		return
	}

	// Packument metadata request: <pkg> or @scope/<pkg>
	storeKey := "npm/metadata/" + storage.CleanKey(decoded) + "/index.json"
	if obj, err := h.store.Get(r.Context(), storeKey); err == nil {
		defer obj.Body.Close()
		data, readErr := io.ReadAll(obj.Body)
		if readErr != nil {
			http.Error(w, readErr.Error(), http.StatusBadGateway)
			return
		}
		rewritten := RewriteNPMPackumentTarballs(data, h.publicBaseURL(r)+"/npm", h.cfg.Upstreams.NPM.UpstreamRegistry)
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Cache-Control", "public, max-age=60, stale-while-revalidate=300")
		w.WriteHeader(http.StatusOK)
		if r.Method == http.MethodGet {
			_, _ = w.Write(rewritten)
		}
		return
	}

	if !h.cfg.Upstreams.NPM.PullThrough {
		http.NotFound(w, r)
		return
	}

	upstreamURL := strings.TrimRight(h.cfg.Upstreams.NPM.UpstreamRegistry, "/") + "/" + raw
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, upstreamURL, nil)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	req.Header.Set("Accept", "application/json")
	resp, err := h.client.Do(req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		http.Error(w, "upstream npm metadata error", resp.StatusCode)
		return
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	rewritten := RewriteNPMPackumentTarballs(data, h.publicBaseURL(r)+"/npm", h.cfg.Upstreams.NPM.UpstreamRegistry)
	_ = h.store.Put(r.Context(), storeKey, bytes.NewReader(rewritten), int64(len(rewritten)), storage.PutOptions{
		ContentType:  "application/json",
		CacheControl: "public, max-age=60, stale-while-revalidate=300",
	})
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=60, stale-while-revalidate=300")
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodGet {
		_, _ = w.Write(rewritten)
	}
}

// RewriteNPMPackumentTarballs rewrites every versions[*].dist.tarball URL to point through mirrorNPMBase.
func RewriteNPMPackumentTarballs(raw []byte, mirrorNPMBase, upstreamBase string) []byte {
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return raw
	}
	mirrorNPMBase = strings.TrimRight(mirrorNPMBase, "/")
	upstreamBase = strings.TrimRight(upstreamBase, "/")
	versions, ok := doc["versions"].(map[string]any)
	if !ok {
		return raw
	}
	for _, v := range versions {
		vmap, ok := v.(map[string]any)
		if !ok {
			continue
		}
		dist, ok := vmap["dist"].(map[string]any)
		if !ok {
			continue
		}
		tb, ok := dist["tarball"].(string)
		if !ok || tb == "" {
			continue
		}
		if upstreamBase != "" && strings.HasPrefix(tb, upstreamBase+"/") {
			dist["tarball"] = mirrorNPMBase + strings.TrimPrefix(tb, upstreamBase)
			continue
		}
		if u, err := url.Parse(tb); err == nil && strings.Contains(u.Path, "/-/") {
			dist["tarball"] = mirrorNPMBase + u.Path
		}
	}
	out, err := json.Marshal(doc)
	if err != nil {
		return raw
	}
	return out
}
