package server

import (
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/ai-workspace-lab/distribution/internal/storage"
)

// handleGitHub serves mirrored Git repositories (/git/<org>/<repo>.git/...),
// GitHub Release assets (/github/<org>/<repo>/releases/download/<tag>/<asset>),
// and raw file proxy (/raw/<org>/<repo>/<ref>/<path>).
func (h *Handler) handleGitHub(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	p := r.URL.Path
	switch {
	case strings.HasPrefix(p, "/git/"):
		key := strings.TrimPrefix(p, "/")
		if _, err := h.store.Head(r.Context(), key); err == nil {
			h.serveObject(w, r, key, h.cfg.CDNRedirect && isImmutablePath(p))
			return
		}
		// Fallback proxy to GitHub HTTPS Git endpoint when not yet mirrored locally.
		rel := strings.TrimPrefix(p, "/git/")
		upstream := strings.TrimRight(h.githubBaseURL(), "/") + "/" + rel
		if r.URL.RawQuery != "" {
			upstream += "?" + r.URL.RawQuery
		}
		h.proxyRead(w, r, upstream, "public, max-age=30")
		return

	case strings.HasPrefix(p, "/github/"):
		// Check local mirrored release asset first: github/<org>/<repo>/releases/download/<tag>/<asset>
		key := strings.TrimPrefix(p, "/")
		if _, err := h.store.Head(r.Context(), key); err == nil {
			h.serveObject(w, r, key, h.cfg.CDNRedirect && isImmutablePath(p))
			return
		} else if !errors.Is(err, storage.ErrNotFound) {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		rel := strings.TrimPrefix(p, "/github/")
		upstream := strings.TrimRight(h.githubBaseURL(), "/") + "/" + rel
		http.Redirect(w, r, upstream, http.StatusFound)
		return

	case strings.HasPrefix(p, "/raw/"):
		key := strings.TrimPrefix(p, "/")
		if _, err := h.store.Head(r.Context(), key); err == nil {
			h.serveObject(w, r, key, false)
			return
		}
		rel := strings.TrimPrefix(p, "/raw/")
		upstream := "https://raw.githubusercontent.com/" + rel
		h.proxyRead(w, r, upstream, "public, max-age=120, stale-while-revalidate=300")
		return
	}
}

func (h *Handler) githubBaseURL() string {
	if h.cfg.Mirrors.GitHub != "" {
		return h.cfg.Mirrors.GitHub
	}
	return "https://github.com"
}

func (h *Handler) proxyRead(w http.ResponseWriter, r *http.Request, upstreamURL, cacheControl string) {
	req, err := http.NewRequestWithContext(r.Context(), r.Method, upstreamURL, nil)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	req.Header.Set("User-Agent", "svc-plus-distribution/2.0")
	resp, err := h.client.Do(req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	if cl := resp.Header.Get("Content-Length"); cl != "" {
		w.Header().Set("Content-Length", cl)
	}
	w.Header().Set("Cache-Control", cacheControl)
	w.WriteHeader(resp.StatusCode)
	if r.Method == http.MethodGet {
		_, _ = io.Copy(w, resp.Body)
	}
}
