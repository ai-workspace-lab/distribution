//go:build !js

package syncer

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"strings"
	"time"

	"github.com/ai-workspace-lab/distribution/internal/config"
	"github.com/ai-workspace-lab/distribution/internal/crypto"
	"github.com/ai-workspace-lab/distribution/internal/storage"
)

// OBSClient is a pure-Go openSUSE Open Build Service (OBS) REST API client + downstream RPM mirror aggregator.
// Requires ZERO `osc` CLI dependency.
type OBSClient struct {
	Store        storage.Backend
	APIBaseURL   string
	DownloadBase string
	Username     string
	Password     string
	Client       *http.Client
}

func NewOBSClient(store storage.Backend, spec config.OBSSourceSpec, client *http.Client) *OBSClient {
	apiBase := spec.APIBaseURL
	if apiBase == "" {
		apiBase = "https://api.opensuse.org"
	}
	dlBase := spec.DownloadBase
	if dlBase == "" {
		dlBase = "https://download.opensuse.org/repositories"
	}
	user := ""
	pass := ""
	if spec.UserEnv != "" {
		user = os.Getenv(spec.UserEnv)
	}
	if spec.PasswordEnv != "" {
		pass = os.Getenv(spec.PasswordEnv)
	}
	if client == nil {
		client = &http.Client{Timeout: 3 * time.Minute}
	}
	return &OBSClient{
		Store:        store,
		APIBaseURL:   strings.TrimRight(apiBase, "/"),
		DownloadBase: strings.TrimRight(dlBase, "/"),
		Username:     user,
		Password:     pass,
		Client:       client,
	}
}

// PutSourceFile uploads a .spec, _service, or source tarball directly to OBS via REST API:
// PUT /source/{project}/{package}/{filename}
func (o *OBSClient) PutSourceFile(ctx context.Context, project, pkg, filename string, data []byte) error {
	u := fmt.Sprintf("%s/source/%s/%s/%s", o.APIBaseURL, url.PathEscape(project), url.PathEscape(pkg), url.PathEscape(filename))
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, u, bytes.NewReader(data))
	if err != nil {
		return err
	}
	if o.Username != "" {
		req.SetBasicAuth(o.Username, o.Password)
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := o.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("OBS PUT %s: HTTP %d: %s", u, resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return nil
}

// TriggerRebuild triggers an OBS package rebuild across all configured Fedora/EL/openSUSE targets:
// POST /build/{project}?cmd=rebuild&package={pkg}
func (o *OBSClient) TriggerRebuild(ctx context.Context, project, pkg string) error {
	u := fmt.Sprintf("%s/build/%s?cmd=rebuild&package=%s", o.APIBaseURL, url.PathEscape(project), url.QueryEscape(pkg))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, nil)
	if err != nil {
		return err
	}
	if o.Username != "" {
		req.SetBasicAuth(o.Username, o.Password)
	}
	resp, err := o.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("OBS rebuild %s/%s: HTTP %d: %s", project, pkg, resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return nil
}

// OBSBuildResult represents a single repository/arch build status from OBS /build/{project}/_result.
type OBSBuildResult struct {
	Repository string `xml:"repository,attr"`
	Arch       string `xml:"arch,attr"`
	State      string `xml:"state,attr"`
	Status     []struct {
		Package string `xml:"package,attr"`
		Code    string `xml:"code,attr"`
	} `xml:"status"`
}

// GetBuildResults queries OBS build status across all repositories & architectures.
func (o *OBSClient) GetBuildResults(ctx context.Context, project, pkg string) ([]OBSBuildResult, error) {
	u := fmt.Sprintf("%s/build/%s/_result?package=%s", o.APIBaseURL, url.PathEscape(project), url.QueryEscape(pkg))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	if o.Username != "" {
		req.SetBasicAuth(o.Username, o.Password)
	}
	resp, err := o.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("OBS _result %s/%s: HTTP %d", project, pkg, resp.StatusCode)
	}
	var doc struct {
		Results []OBSBuildResult `xml:"result"`
	}
	if err := xml.NewDecoder(resp.Body).Decode(&doc); err != nil {
		return nil, err
	}
	return doc.Results, nil
}

// MirrorOBSRPMs pulls built RPMs from download.opensuse.org/repositories/{project}/{repo}/repodata/repomd.xml
// and mirrors them into storage under rpm/packages/obs/<repo>/<file>.rpm.
func (o *OBSClient) MirrorOBSRPMs(ctx context.Context, spec config.OBSSourceSpec) ([]RPMPackage, error) {
	if !spec.MirrorEnabled {
		return nil, nil
	}
	projPath := strings.ReplaceAll(spec.Project, ":", ":/")
	var out []RPMPackage
	for _, repo := range spec.Repositories {
		repoBase := fmt.Sprintf("%s/%s/%s", o.DownloadBase, projPath, repo)
		repomdBytes, err := o.httpGet(ctx, repoBase+"/repodata/repomd.xml")
		if err != nil {
			return out, err
		}
		primaryHref, err := parsePrimaryHrefFromRepomd(repomdBytes)
		if err != nil {
			return out, err
		}
		primaryRaw, err := o.httpGet(ctx, repoBase+"/"+strings.TrimLeft(primaryHref, "/"))
		if err != nil {
			return out, err
		}
		var xmlBytes []byte
		if strings.HasSuffix(primaryHref, ".gz") {
			gz, err := gzip.NewReader(bytes.NewReader(primaryRaw))
			if err != nil {
				return out, err
			}
			xmlBytes, err = io.ReadAll(gz)
			_ = gz.Close()
			if err != nil {
				return out, err
			}
		} else {
			xmlBytes = primaryRaw
		}

		pkgs, err := parsePrimaryXMLPackages(xmlBytes)
		if err != nil {
			return out, err
		}
		for _, p := range pkgs {
			if len(spec.Architectures) > 0 && !containsStr(spec.Architectures, p.Arch) && p.Arch != "noarch" {
				continue
			}
			rpmURL := repoBase + "/" + strings.TrimLeft(p.Location, "/")
			destRel := fmt.Sprintf("packages/obs/%s/%s", storage.CleanKey(repo), path.Base(p.Location))
			storeKey := "rpm/" + destRel
			rpmBytes, err := o.fetchOrDownloadRPM(ctx, rpmURL, storeKey)
			if err != nil {
				return out, err
			}
			p.Location = destRel
			p.Digests = crypto.ComputeDigestsBytes(rpmBytes)
			out = append(out, p)
		}
	}
	return out, nil
}

func (o *OBSClient) fetchOrDownloadRPM(ctx context.Context, srcURL, storeKey string) ([]byte, error) {
	if obj, err := o.Store.Get(ctx, storeKey); err == nil {
		defer obj.Body.Close()
		return io.ReadAll(obj.Body)
	}
	data, err := o.httpGet(ctx, srcURL)
	if err != nil {
		return nil, err
	}
	if err := o.Store.Put(ctx, storeKey, bytes.NewReader(data), int64(len(data)), storage.PutOptions{
		ContentType:  "application/x-rpm",
		CacheControl: "public, max-age=31536000, immutable",
	}); err != nil {
		return nil, err
	}
	return data, nil
}

func (o *OBSClient) httpGet(ctx context.Context, u string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "svc-plus-distribution/2.0")
	resp, err := o.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: HTTP %d", u, resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}

func parsePrimaryHrefFromRepomd(data []byte) (string, error) {
	var doc struct {
		Data []struct {
			Type     string `xml:"type,attr"`
			Location struct {
				Href string `xml:"href,attr"`
			} `xml:"location"`
		} `xml:"data"`
	}
	if err := xml.Unmarshal(data, &doc); err != nil {
		return "", err
	}
	for _, d := range doc.Data {
		if d.Type == "primary" && d.Location.Href != "" {
			return d.Location.Href, nil
		}
	}
	return "", fmt.Errorf("primary metadata entry not found in repomd.xml")
}

func parsePrimaryXMLPackages(data []byte) ([]RPMPackage, error) {
	var doc struct {
		Packages []struct {
			Name        string `xml:"name"`
			Arch        string `xml:"arch"`
			Summary     string `xml:"summary"`
			Description string `xml:"description"`
			URL         string `xml:"url"`
			Version     struct {
				Epoch string `xml:"epoch,attr"`
				Ver   string `xml:"ver,attr"`
				Rel   string `xml:"rel,attr"`
			} `xml:"version"`
			Location struct {
				Href string `xml:"href,attr"`
			} `xml:"location"`
		} `xml:"package"`
	}
	if err := xml.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	out := make([]RPMPackage, 0, len(doc.Packages))
	for _, p := range doc.Packages {
		out = append(out, RPMPackage{
			Name:        p.Name,
			Arch:        p.Arch,
			Epoch:       p.Version.Epoch,
			Version:     p.Version.Ver,
			Release:     p.Version.Rel,
			Summary:     p.Summary,
			Description: p.Description,
			URL:         p.URL,
			Location:    p.Location.Href,
		})
	}
	return out, nil
}

func containsStr(list []string, target string) bool {
	for _, item := range list {
		if item == target {
			return true
		}
	}
	return false
}
