//go:build !js

package syncer

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/ai-workspace-lab/distribution/internal/config"
	"github.com/ai-workspace-lab/distribution/internal/crypto"
	"github.com/ai-workspace-lab/distribution/internal/storage"
)

// DebianPPASyncer handles BOTH:
//   1) Downstream mirror aggregation from Ubuntu PPA (ppa.launchpadcontent.net) & Debian buildd/APT repos
//   2) Upstream source package signing (.dsc / .changes) & dput submission to Launchpad PPA / Debian Mentors / buildd
type DebianPPASyncer struct {
	Store  storage.Backend
	Signer *crypto.Signer
	Client *http.Client
}

func NewDebianPPASyncer(store storage.Backend, signer *crypto.Signer, client *http.Client) *DebianPPASyncer {
	if client == nil {
		client = &http.Client{Timeout: 3 * time.Minute}
	}
	return &DebianPPASyncer{Store: store, Signer: signer, Client: client}
}

// MirrorUbuntuPPA pulls built .deb packages from Launchpad PPA and mirrors them into apt/pool/ppa/<name>/...
func (d *DebianPPASyncer) MirrorUbuntuPPA(ctx context.Context, spec config.UbuntuPPASpec) ([]DebPackage, error) {
	if !spec.MirrorEnabled {
		return nil, nil
	}
	baseURL := fmt.Sprintf("https://ppa.launchpadcontent.net/%s/%s/ubuntu", spec.Owner, spec.PPAName)
	return d.mirrorAPTRepo(ctx, baseURL, "ppa/"+spec.Name, spec.Series, []string{"main"}, spec.Architectures)
}

// MirrorDebianBuildd pulls built .deb packages from a Debian buildd / mini-buildd / upstream Debian archive.
func (d *DebianPPASyncer) MirrorDebianBuildd(ctx context.Context, spec config.DebianBuilddSpec) ([]DebPackage, error) {
	if !spec.MirrorEnabled || spec.BaseURL == "" {
		return nil, nil
	}
	return d.mirrorAPTRepo(ctx, strings.TrimRight(spec.BaseURL, "/"), "debian/"+spec.Name, spec.Suites, spec.Components, spec.Architectures)
}

func (d *DebianPPASyncer) mirrorAPTRepo(ctx context.Context, baseURL, poolPrefix string, suites, components, archs []string) ([]DebPackage, error) {
	if len(components) == 0 {
		components = []string{"main"}
	}
	if len(archs) == 0 {
		archs = []string{"amd64", "arm64"}
	}
	var mirrored []DebPackage
	seen := map[string]bool{}

	for _, suite := range suites {
		for _, comp := range components {
			for _, arch := range archs {
				stanzas, err := d.fetchPackagesIndex(ctx, baseURL, suite, comp, arch)
				if err != nil {
					return mirrored, err
				}
				for _, fields := range stanzas {
					relFile := fields["Filename"]
					if relFile == "" {
						continue
					}
					srcURL := strings.TrimRight(baseURL, "/") + "/" + strings.TrimLeft(relFile, "/")
					destRel := fmt.Sprintf("pool/%s/%s/%s", storage.CleanKey(poolPrefix), suite, path.Base(relFile))
					if seen[destRel] {
						continue
					}
					seen[destRel] = true

					debBytes, err := d.fetchOrDownloadDeb(ctx, srcURL, "apt/"+destRel)
					if err != nil {
						return mirrored, err
					}
					dig := crypto.ComputeDigestsBytes(debBytes)
					mirrored = append(mirrored, DebPackage{
						Package:      fields["Package"],
						Version:      fields["Version"],
						Architecture: fields["Architecture"],
						Maintainer:   fields["Maintainer"],
						Section:      fields["Section"],
						Priority:     fields["Priority"],
						Depends:      fields["Depends"],
						Homepage:     fields["Homepage"],
						Description:  fields["Description"],
						Filename:     destRel,
						Digests:      dig,
					})
				}
			}
		}
	}
	return mirrored, nil
}

func (d *DebianPPASyncer) fetchPackagesIndex(ctx context.Context, baseURL, suite, comp, arch string) ([]map[string]string, error) {
	gzURL := fmt.Sprintf("%s/dists/%s/%s/binary-%s/Packages.gz", baseURL, suite, comp, arch)
	raw, err := d.httpGetBytes(ctx, gzURL)
	if err == nil {
		gz, gzErr := gzip.NewReader(bytes.NewReader(raw))
		if gzErr != nil {
			return nil, gzErr
		}
		defer gz.Close()
		plain, rErr := io.ReadAll(gz)
		if rErr != nil {
			return nil, rErr
		}
		return parseDebianPackagesList(string(plain)), nil
	}
	plainURL := fmt.Sprintf("%s/dists/%s/%s/binary-%s/Packages", baseURL, suite, comp, arch)
	plain, err := d.httpGetBytes(ctx, plainURL)
	if err != nil {
		return nil, err
	}
	return parseDebianPackagesList(string(plain)), nil
}

func (d *DebianPPASyncer) fetchOrDownloadDeb(ctx context.Context, srcURL, storeKey string) ([]byte, error) {
	if obj, err := d.Store.Get(ctx, storeKey); err == nil {
		defer obj.Body.Close()
		return io.ReadAll(obj.Body)
	}
	data, err := d.httpGetBytes(ctx, srcURL)
	if err != nil {
		return nil, err
	}
	if err := d.Store.Put(ctx, storeKey, bytes.NewReader(data), int64(len(data)), storage.PutOptions{
		ContentType:  "application/vnd.debian.binary-package",
		CacheControl: "public, max-age=31536000, immutable",
	}); err != nil {
		return nil, err
	}
	return data, nil
}

func (d *DebianPPASyncer) httpGetBytes(ctx context.Context, u string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "svc-plus-distribution/2.0")
	resp, err := d.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: HTTP %d", u, resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}

func parseDebianPackagesList(raw string) []map[string]string {
	blocks := strings.Split(strings.ReplaceAll(raw, "\r\n", "\n"), "\n\n")
	var out []map[string]string
	for _, b := range blocks {
		if strings.TrimSpace(b) == "" {
			continue
		}
		st := parseDebianStanza(b)
		if st["Package"] != "" {
			out = append(out, st)
		}
	}
	return out
}

// RenderDputConfig generates a dput.cf snippet for Ubuntu PPA (Launchpad) or Debian buildd/Mentors uploads.
func RenderDputConfig(targetName, fqdn, incoming, method string) string {
	if method == "" {
		method = "ftp"
	}
	return fmt.Sprintf(`[%s]
fqdn = %s
method = %s
incoming = %s
login = anonymous
allow_unsigned_uploads = 0
`, targetName, fqdn, method, incoming)
}

// SubmitSourceChanges clearsigns a Debian .changes file in pure Go (when Signer is set) and uploads it via `dput`
// to Ubuntu Launchpad PPA (`ppa:<owner>/<ppa>`) or a Debian buildd/Mentors upload queue.
func (d *DebianPPASyncer) SubmitSourceChanges(ctx context.Context, changesPath, targetHost, incoming, method string, dryRun bool) (string, error) {
	raw, err := os.ReadFile(changesPath)
	if err != nil {
		return "", fmt.Errorf("read changes file %s: %w", changesPath, err)
	}
	if d.Signer != nil && !strings.Contains(string(raw), "-----BEGIN PGP SIGNED MESSAGE-----") {
		signed, err := d.Signer.ClearSign(raw)
		if err != nil {
			return "", fmt.Errorf("clearsign %s: %w", changesPath, err)
		}
		if err := os.WriteFile(changesPath, signed, 0o644); err != nil {
			return "", err
		}
	}

	dputCfg := RenderDputConfig("distd-target", targetHost, incoming, method)
	if dryRun {
		return fmt.Sprintf("[dry-run] would submit %s via dput to %s%s\n%s", filepath.Base(changesPath), targetHost, incoming, dputCfg), nil
	}

	dputBin, err := exec.LookPath("dput")
	if err != nil {
		return "", fmt.Errorf("dput binary not found in PATH (install dput-ng or dput): %w", err)
	}
	tmpCfg, err := os.CreateTemp("", "distd-dput-*.cf")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmpCfg.Name())
	_, _ = tmpCfg.WriteString(dputCfg)
	_ = tmpCfg.Close()

	cmd := exec.CommandContext(ctx, dputBin, "-c", tmpCfg.Name(), "-u", "distd-target", changesPath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("dput submit failed: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}
