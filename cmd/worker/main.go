//go:build js && wasm

package main

import (
	"strings"

	"github.com/ai-workspace-lab/distribution/internal/config"
	"github.com/ai-workspace-lab/distribution/internal/server"
	"github.com/ai-workspace-lab/distribution/internal/storage"
	"github.com/syumai/workers-go"
	"github.com/syumai/workers-go/cloudflare"
)

func main() {
	cfg := config.DefaultConfig()
	if v := cloudflare.Getenv("MIRROR_NAME"); v != "" {
		cfg.MirrorName = v
	}
	if v := cloudflare.Getenv("PUBLIC_URL"); v != "" {
		cfg.PublicURL = v
	}
	if v := cloudflare.Getenv("DL_URL"); v != "" {
		cfg.DLURL = v
	}
	if v := cloudflare.Getenv("HARBOR_URL"); v != "" {
		cfg.Upstreams.OCI.HarborURL = v
	}
	if v := cloudflare.Getenv("NPM_UPSTREAM"); v != "" {
		cfg.Upstreams.NPM.UpstreamRegistry = v
	}
	if v := cloudflare.Getenv("CDN_REDIRECT"); strings.EqualFold(v, "false") {
		cfg.CDNRedirect = false
	}

	r2Binding := cloudflare.Getenv("R2_BUCKET_BINDING")
	if r2Binding == "" {
		r2Binding = "REPO_BUCKET"
	}
	publicBase := cloudflare.Getenv("R2_PUBLIC_BASE_URL")
	var store storage.Backend
	if b, err := storage.NewR2WorkerBackend(r2Binding, publicBase); err == nil {
		store = b
	} else {
		store = storage.NewMemoryBackend(publicBase)
	}

	handler := server.NewHandler(cfg, store, nil)
	workers.Serve(handler)
}
