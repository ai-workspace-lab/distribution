package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/ai-workspace-lab/distribution/internal/storage"
)

// WinGetIndex is the prebuilt static JSON index stored at winget/api/index.json so
// WinGet REST Source API v1.x runs with zero database on Cloudflare Workers and locally.
type WinGetIndex struct {
	SourceIdentifier string                `json:"sourceIdentifier"`
	Packages         []WinGetPackageRecord `json:"packages"`
}

type WinGetPackageRecord struct {
	PackageIdentifier string                  `json:"PackageIdentifier"`
	PackageName       string                  `json:"PackageName"`
	Publisher         string                  `json:"Publisher"`
	Moniker           string                  `json:"Moniker,omitempty"`
	Tags              []string                `json:"Tags,omitempty"`
	ShortDescription  string                  `json:"ShortDescription,omitempty"`
	Homepage          string                  `json:"Homepage,omitempty"`
	License           string                  `json:"License,omitempty"`
	Versions          []WinGetVersionManifest `json:"Versions"`
}

type WinGetVersionManifest struct {
	PackageVersion string               `json:"PackageVersion"`
	DefaultLocale  WinGetLocaleMetadata `json:"DefaultLocale"`
	Installers     []WinGetInstaller    `json:"Installers"`
}

type WinGetLocaleMetadata struct {
	PackageLocale    string   `json:"PackageLocale"`
	Publisher        string   `json:"Publisher"`
	PackageName      string   `json:"PackageName"`
	License          string   `json:"License"`
	ShortDescription string   `json:"ShortDescription"`
	PackageUrl       string   `json:"PackageUrl,omitempty"`
	Moniker          string   `json:"Moniker,omitempty"`
	Tags             []string `json:"Tags,omitempty"`
}

type WinGetInstaller struct {
	Architecture    string `json:"Architecture"`
	InstallerType   string `json:"InstallerType"`
	InstallerUrl    string `json:"InstallerUrl"`
	CNInstallerUrl  string `json:"CNInstallerUrl,omitempty"`
	InstallerSha256 string `json:"InstallerSha256"`
}

// handleWinGetAPI implements Microsoft's WinGet REST Source API (v1.4 - v1.7):
//   - GET  /winget/api/information
//   - POST /winget/api/manifestSearch
//   - GET  /winget/api/packageManifests/{packageIdentifier}
func (h *Handler) handleWinGetAPI(w http.ResponseWriter, r *http.Request) {
	sub := strings.TrimPrefix(r.URL.Path, "/winget/api")
	sub = strings.TrimPrefix(sub, "/")

	switch {
	case sub == "" || sub == "information":
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		h.writeJSON(w, http.StatusOK, map[string]any{
			"Data": map[string]any{
				"SourceIdentifier":        h.cfg.Upstreams.WinGet.SourceIdentifier,
				"ServerSupportedVersions": []string{"1.4.0", "1.5.0", "1.6.0", "1.7.0"},
			},
		}, "public, max-age=300")
		return

	case sub == "manifestSearch":
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			Query struct {
				KeyWord   string `json:"KeyWord"`
				MatchType string `json:"MatchType"`
			} `json:"Query"`
			Inclusions []struct {
				PackageMatchField string `json:"PackageMatchField"`
				RequestMatch      struct {
					KeyWord   string `json:"KeyWord"`
					MatchType string `json:"MatchType"`
				} `json:"RequestMatch"`
			} `json:"Inclusions"`
		}
		_ = json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req)

		idx, err := h.loadWinGetIndex(r.Context())
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}

		kw := strings.ToLower(strings.TrimSpace(req.Query.KeyWord))
		var matches []map[string]any
		for _, pkg := range idx.Packages {
			if !winGetMatches(pkg, kw, req.Inclusions) {
				continue
			}
			vers := make([]map[string]any, 0, len(pkg.Versions))
			for _, v := range pkg.Versions {
				vers = append(vers, map[string]any{
					"PackageVersion": v.PackageVersion,
				})
			}
			matches = append(matches, map[string]any{
				"PackageIdentifier": pkg.PackageIdentifier,
				"PackageName":       pkg.PackageName,
				"Publisher":         pkg.Publisher,
				"Versions":          vers,
			})
		}
		if len(matches) == 0 {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		h.writeJSON(w, http.StatusOK, map[string]any{"Data": matches}, "public, max-age=60")
		return

	case strings.HasPrefix(sub, "packageManifests/"):
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		id := strings.TrimPrefix(sub, "packageManifests/")
		idx, err := h.loadWinGetIndex(r.Context())
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		for _, pkg := range idx.Packages {
			if strings.EqualFold(pkg.PackageIdentifier, id) {
				versionsOut := make([]map[string]any, 0, len(pkg.Versions))
				for _, v := range pkg.Versions {
					installers := make([]map[string]any, 0, len(v.Installers))
					for _, inst := range v.Installers {
						u := inst.InstallerUrl
						if h.mirrorName() == "cn" && inst.CNInstallerUrl != "" {
							u = inst.CNInstallerUrl
						}
						installers = append(installers, map[string]any{
							"Architecture":    inst.Architecture,
							"InstallerType":   inst.InstallerType,
							"InstallerUrl":    u,
							"InstallerSha256": inst.InstallerSha256,
						})
					}
					versionsOut = append(versionsOut, map[string]any{
						"PackageVersion": v.PackageVersion,
						"DefaultLocale":  v.DefaultLocale,
						"Installers":     installers,
					})
				}
				h.writeJSON(w, http.StatusOK, map[string]any{
					"Data": map[string]any{
						"PackageIdentifier": pkg.PackageIdentifier,
						"Versions":          versionsOut,
					},
				}, "public, max-age=60, stale-while-revalidate=300")
				return
			}
		}
		http.NotFound(w, r)
		return
	default:
		http.NotFound(w, r)
	}
}

func (h *Handler) loadWinGetIndex(ctx context.Context) (*WinGetIndex, error) {
	obj, err := h.store.Get(ctx, "winget/api/index.json")
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return &WinGetIndex{SourceIdentifier: h.cfg.Upstreams.WinGet.SourceIdentifier}, nil
		}
		return nil, err
	}
	defer obj.Body.Close()
	var idx WinGetIndex
	if err := json.NewDecoder(obj.Body).Decode(&idx); err != nil {
		return nil, err
	}
	return &idx, nil
}

func winGetMatches(pkg WinGetPackageRecord, kw string, inclusions []struct {
	PackageMatchField string `json:"PackageMatchField"`
	RequestMatch      struct {
		KeyWord   string `json:"KeyWord"`
		MatchType string `json:"MatchType"`
	} `json:"RequestMatch"`
}) bool {
	if kw != "" {
		haystack := strings.ToLower(strings.Join([]string{
			pkg.PackageIdentifier, pkg.PackageName, pkg.Publisher, pkg.Moniker, pkg.ShortDescription, strings.Join(pkg.Tags, " "),
		}, " "))
		if !strings.Contains(haystack, kw) {
			return false
		}
	}
	if len(inclusions) == 0 {
		return true
	}
	for _, inc := range inclusions {
		needle := strings.ToLower(strings.TrimSpace(inc.RequestMatch.KeyWord))
		if needle == "" {
			return true
		}
		switch strings.ToLower(inc.PackageMatchField) {
		case "packageidentifier":
			if strings.Contains(strings.ToLower(pkg.PackageIdentifier), needle) {
				return true
			}
		case "packagename":
			if strings.Contains(strings.ToLower(pkg.PackageName), needle) {
				return true
			}
		case "moniker":
			if strings.Contains(strings.ToLower(pkg.Moniker), needle) {
				return true
			}
		default:
			if strings.Contains(strings.ToLower(pkg.PackageIdentifier+" "+pkg.PackageName+" "+pkg.Moniker), needle) {
				return true
			}
		}
	}
	return false
}
