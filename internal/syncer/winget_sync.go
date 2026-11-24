//go:build !js

package syncer

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/ai-workspace-lab/distribution/internal/server"
	"github.com/ai-workspace-lab/distribution/internal/storage"
)

// WinGetSyncer generates both WinGet 1.6+ YAML manifests (for static repo & microsoft/winget-pkgs PRs)
// and the zero-DB JSON index (winget/api/index.json) served by the WinGet REST Source API on CF Workers & local distd.
type WinGetSyncer struct {
	Store            storage.Backend
	SourceIdentifier string
}

func NewWinGetSyncer(store storage.Backend, sourceID string) *WinGetSyncer {
	if sourceID == "" {
		sourceID = "SvcPlus.Distribution"
	}
	return &WinGetSyncer{Store: store, SourceIdentifier: sourceID}
}

// Publish writes all static WinGet YAML manifests and the consolidated winget/api/index.json to storage.
func (w *WinGetSyncer) Publish(ctx context.Context, packages []server.WinGetPackageRecord) error {
	for _, pkg := range packages {
		parts := strings.SplitN(pkg.PackageIdentifier, ".", 2)
		if len(parts) != 2 {
			continue
		}
		publisher, app := parts[0], parts[1]
		firstLetter := strings.ToLower(publisher[:1])
		for _, ver := range pkg.Versions {
			baseDir := fmt.Sprintf("winget/manifests/%s/%s/%s/%s", firstLetter, publisher, app, ver.PackageVersion)

			versionYAML := fmt.Sprintf(`# yaml-language-server: $schema=https://aka.ms/winget-manifest.version.1.6.0.schema.json
PackageIdentifier: %s
PackageVersion: %s
DefaultLocale: %s
ManifestType: version
ManifestVersion: 1.6.0
`, pkg.PackageIdentifier, ver.PackageVersion, defaultStr(ver.DefaultLocale.PackageLocale, "en-US"))

			var instBuf strings.Builder
			fmt.Fprintf(&instBuf, `# yaml-language-server: $schema=https://aka.ms/winget-manifest.installer.1.6.0.schema.json
PackageIdentifier: %s
PackageVersion: %s
Installers:
`, pkg.PackageIdentifier, ver.PackageVersion)
			for _, inst := range ver.Installers {
				fmt.Fprintf(&instBuf, "  - Architecture: %s\n    InstallerType: %s\n    InstallerUrl: %s\n    InstallerSha256: %s\n",
					defaultStr(inst.Architecture, "x64"),
					defaultStr(inst.InstallerType, "msi"),
					inst.InstallerUrl,
					strings.ToUpper(inst.InstallerSha256))
			}
			instBuf.WriteString("ManifestType: installer\nManifestVersion: 1.6.0\n")

			localeYAML := fmt.Sprintf(`# yaml-language-server: $schema=https://aka.ms/winget-manifest.defaultLocale.1.6.0.schema.json
PackageIdentifier: %s
PackageVersion: %s
PackageLocale: %s
Publisher: %s
PackageName: %s
License: %s
ShortDescription: %s
PackageUrl: %s
Moniker: %s
ManifestType: defaultLocale
ManifestVersion: 1.6.0
`, pkg.PackageIdentifier, ver.PackageVersion,
				defaultStr(ver.DefaultLocale.PackageLocale, "en-US"),
				defaultStr(ver.DefaultLocale.Publisher, pkg.Publisher),
				defaultStr(ver.DefaultLocale.PackageName, pkg.PackageName),
				defaultStr(ver.DefaultLocale.License, "Apache-2.0"),
				defaultStr(ver.DefaultLocale.ShortDescription, pkg.ShortDescription),
				defaultStr(ver.DefaultLocale.PackageUrl, pkg.Homepage),
				defaultStr(ver.DefaultLocale.Moniker, pkg.Moniker))

			files := map[string]string{
				baseDir + "/" + pkg.PackageIdentifier + ".yaml":               versionYAML,
				baseDir + "/" + pkg.PackageIdentifier + ".installer.yaml":     instBuf.String(),
				baseDir + "/" + pkg.PackageIdentifier + ".locale.en-US.yaml": localeYAML,
			}
			for key, content := range files {
				if err := w.Store.Put(ctx, key, strings.NewReader(content), int64(len(content)), storage.PutOptions{
					ContentType:  "application/yaml",
					CacheControl: "public, max-age=300",
				}); err != nil {
					return err
				}
			}
		}
	}

	idx := server.WinGetIndex{
		SourceIdentifier: w.SourceIdentifier,
		Packages:         packages,
	}
	raw, err := json.MarshalIndent(idx, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	return w.Store.Put(ctx, "winget/api/index.json", bytes.NewReader(raw), int64(len(raw)), storage.PutOptions{
		ContentType:  "application/json",
		CacheControl: "public, max-age=60, stale-while-revalidate=300",
	})
}

func defaultStr(v, fallback string) string {
	if strings.TrimSpace(v) == "" {
		return fallback
	}
	return v
}
