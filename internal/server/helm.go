package server

import (
	"net/http"
	"strings"
)

// handleHelm serves classic HTTP Helm chart repositories (/helm/index.yaml and /helm/charts/*.tgz).
// OCI Helm charts (`helm pull oci://repo.svc.plus/charts/...`) are served transparently by handleOCI (/v2/...).
func (h *Handler) handleHelm(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	p := strings.TrimPrefix(r.URL.Path, "/")
	if p == "helm" || p == "helm/" {
		p = "helm/index.yaml"
	}
	allowRedirect := h.cfg.CDNRedirect && strings.HasSuffix(strings.ToLower(p), ".tgz")
	h.serveObject(w, r, p, allowRedirect)
}
