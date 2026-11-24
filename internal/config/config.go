package config

import (
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// Config represents the unified distribution configuration (mirrors, storage, upstreams, and catalog products).
type Config struct {
	SchemaVersion int                    `yaml:"schemaVersion" json:"schemaVersion"`
	MirrorName    string                 `yaml:"mirrorName,omitempty" json:"mirrorName,omitempty"`
	PublicURL     string                 `yaml:"publicUrl,omitempty" json:"publicUrl,omitempty"`
	DLURL         string                 `yaml:"dlUrl,omitempty" json:"dlUrl,omitempty"`
	CDNRedirect   bool                   `yaml:"cdnRedirect,omitempty" json:"cdnRedirect,omitempty"`
	Mirrors       MirrorsConfig          `yaml:"mirrors,omitempty" json:"mirrors,omitempty"`
	Storage       StorageConfig          `yaml:"storage,omitempty" json:"storage,omitempty"`
	Upstreams     UpstreamsConfig        `yaml:"upstreams,omitempty" json:"upstreams,omitempty"`
	Products      map[string]ProductSpec `yaml:"products,omitempty" json:"products,omitempty"`
}

type MirrorsConfig struct {
	DL      string                  `yaml:"dl" json:"dl"`
	CNDL    string                  `yaml:"cn_dl" json:"cn_dl"`
	Repo    string                  `yaml:"repo" json:"repo"`
	CNRepo  string                  `yaml:"cn_repo" json:"cn_repo"`
	GitHub  string                  `yaml:"github" json:"github"`
	Regions map[string]RegionMirror `yaml:"regions" json:"regions"`
}

type RegionMirror struct {
	Label           string            `yaml:"label" json:"label"`
	DL              string            `yaml:"dl" json:"dl"`
	Repo            string            `yaml:"repo" json:"repo"`
	HomebrewTapRepo string            `yaml:"homebrew_tap_repo" json:"homebrew_tap_repo"`
	HomebrewTapURL  string            `yaml:"homebrew_tap_url" json:"homebrew_tap_url"`
	HomebrewTapHTTP string            `yaml:"homebrew_tap_http" json:"homebrew_tap_http"`
	ScoopBucketRepo string            `yaml:"scoop_bucket_repo" json:"scoop_bucket_repo"`
	ScoopBucketURL  string            `yaml:"scoop_bucket_url" json:"scoop_bucket_url"`
	ScoopBucketHTTP string            `yaml:"scoop_bucket_http" json:"scoop_bucket_http"`
	Storage         StorageTargetSpec `yaml:"storage" json:"storage"`
}

type StorageConfig struct {
	Default string                       `yaml:"default" json:"default"`
	Targets map[string]StorageTargetSpec `yaml:"targets" json:"targets"`
}

type StorageTargetSpec struct {
	Provider        string `yaml:"provider" json:"provider"` // r2 | cloudflare-r2 | oss | aliyun-oss | s3 | local | memory
	Bucket          string `yaml:"bucket,omitempty" json:"bucket,omitempty"`
	Endpoint        string `yaml:"endpoint,omitempty" json:"endpoint,omitempty"`
	Region          string `yaml:"region,omitempty" json:"region,omitempty"`
	Prefix          string `yaml:"prefix,omitempty" json:"prefix,omitempty"`
	PublicBaseURL   string `yaml:"public_base_url,omitempty" json:"publicBaseUrl,omitempty"`
	LocalDir        string `yaml:"local_dir,omitempty" json:"localDir,omitempty"`
	AccessKeyEnv    string `yaml:"access_key_env,omitempty" json:"accessKeyEnv,omitempty"`
	SecretKeyEnv    string `yaml:"secret_key_env,omitempty" json:"secretKeyEnv,omitempty"`
	ForcePathStyle  bool   `yaml:"force_path_style,omitempty" json:"forcePathStyle,omitempty"`
	WorkerR2Binding string `yaml:"worker_r2_binding,omitempty" json:"workerR2Binding,omitempty"`
}

type UpstreamsConfig struct {
	Files  []FileUpstreamSpec   `yaml:"files,omitempty" json:"files,omitempty"`
	NPM    NPMUpstreamConfig    `yaml:"npm,omitempty" json:"npm,omitempty"`
	GitHub GitHubUpstreamConfig `yaml:"github,omitempty" json:"github,omitempty"`
	OCI    OCIUpstreamConfig    `yaml:"oci,omitempty" json:"oci,omitempty"`
	Helm   HelmUpstreamConfig   `yaml:"helm,omitempty" json:"helm,omitempty"`
	APT    APTUpstreamConfig    `yaml:"apt,omitempty" json:"apt,omitempty"`
	RPM    RPMUpstreamConfig    `yaml:"rpm,omitempty" json:"rpm,omitempty"`
	WinGet WinGetUpstreamConfig `yaml:"winget,omitempty" json:"winget,omitempty"`
}

type FileUpstreamSpec struct {
	Name      string `yaml:"name" json:"name"`
	Type      string `yaml:"type" json:"type"` // https | rsync
	SourceURL string `yaml:"source_url" json:"sourceUrl"`
	DestPath  string `yaml:"dest_path" json:"destPath"`
	Include   string `yaml:"include,omitempty" json:"include,omitempty"`
}

type NPMUpstreamConfig struct {
	UpstreamRegistry string   `yaml:"upstream_registry" json:"upstreamRegistry"`
	PullThrough      bool     `yaml:"pull_through" json:"pullThrough"`
	Packages         []string `yaml:"packages,omitempty" json:"packages,omitempty"`
	Scopes           []string `yaml:"scopes,omitempty" json:"scopes,omitempty"`
}

type GitHubUpstreamConfig struct {
	APIBaseURL string             `yaml:"api_base_url,omitempty" json:"apiBaseUrl,omitempty"`
	TokenEnv   string             `yaml:"token_env,omitempty" json:"tokenEnv,omitempty"`
	Repos      []GitHubRepoMirror `yaml:"repos,omitempty" json:"repos,omitempty"`
}

type GitHubRepoMirror struct {
	Repo          string   `yaml:"repo" json:"repo"` // owner/name
	MirrorGit     bool     `yaml:"mirror_git" json:"mirrorGit"`
	MirrorRelease bool     `yaml:"mirror_release" json:"mirrorRelease"`
	ProductID     string   `yaml:"product_id,omitempty" json:"productId,omitempty"`
	GHCRImages    []string `yaml:"ghcr_images,omitempty" json:"ghcrImages,omitempty"`
}

type OCIUpstreamConfig struct {
	// Mode: "standalone" (R2/OSS/Local OCI blob layout), "harbor" (sync to Harbor + 302 CDN), "hybrid" (both)
	Mode               string            `yaml:"mode" json:"mode"`
	HarborURL          string            `yaml:"harbor_url,omitempty" json:"harborUrl,omitempty"`
	HarborProject      string            `yaml:"harbor_project,omitempty" json:"harborProject,omitempty"`
	HarborUserEnv      string            `yaml:"harbor_user_env,omitempty" json:"harborUserEnv,omitempty"`
	HarborPasswordEnv  string            `yaml:"harbor_password_env,omitempty" json:"harborPasswordEnv,omitempty"`
	BlobRedirectBase   string            `yaml:"blob_redirect_base,omitempty" json:"blobRedirectBase,omitempty"`
	PullThroughEnabled bool              `yaml:"pull_through_enabled" json:"pullThroughEnabled"`
	RegistryAliases    map[string]string `yaml:"registry_aliases,omitempty" json:"registryAliases,omitempty"` // e.g. ghcr -> ghcr.io, docker -> registry-1.docker.io
	Mirrors            []OCIImageMirror  `yaml:"mirrors,omitempty" json:"mirrors,omitempty"`
}

type OCIImageMirror struct {
	Source      string   `yaml:"source" json:"source"`           // e.g. ghcr.io/ai-workspace-services/console
	Target      string   `yaml:"target" json:"target"`           // e.g. svc-plus/console
	Tags        []string `yaml:"tags,omitempty" json:"tags,omitempty"`
	SyncToHarbor bool    `yaml:"sync_to_harbor,omitempty" json:"syncToHarbor,omitempty"`
	StoreInR2   bool     `yaml:"store_in_r2,omitempty" json:"storeInR2,omitempty"`
}

type HelmUpstreamConfig struct {
	Mode       string            `yaml:"mode" json:"mode"` // oci | classic | dual
	OCIProject string            `yaml:"oci_project,omitempty" json:"ociProject,omitempty"`
	Repos      []HelmRepoMirror  `yaml:"repos,omitempty" json:"repos,omitempty"`
	OCICharts  []OCIImageMirror  `yaml:"oci_charts,omitempty" json:"ociCharts,omitempty"`
}

type HelmRepoMirror struct {
	Name   string   `yaml:"name" json:"name"`
	URL    string   `yaml:"url" json:"url"`
	Charts []string `yaml:"charts,omitempty" json:"charts,omitempty"`
}

type APTUpstreamConfig struct {
	Distributions []string            `yaml:"distributions,omitempty" json:"distributions,omitempty"`
	Components    []string            `yaml:"components,omitempty" json:"components,omitempty"`
	Architectures []string            `yaml:"architectures,omitempty" json:"architectures,omitempty"`
	SigningKeyEnv string              `yaml:"signing_key_env,omitempty" json:"signingKeyEnv,omitempty"`
	PPAs          []UbuntuPPASpec     `yaml:"ppas,omitempty" json:"ppas,omitempty"`
	DebianRepos   []DebianBuilddSpec  `yaml:"debian_repos,omitempty" json:"debianRepos,omitempty"`
}

type UbuntuPPASpec struct {
	Name          string   `yaml:"name" json:"name"`
	Owner         string   `yaml:"owner" json:"owner"`     // Launchpad user/team
	PPAName       string   `yaml:"ppa_name" json:"ppaName"` // e.g. svc-plus
	Series        []string `yaml:"series" json:"series"`   // e.g. jammy, noble
	Architectures []string `yaml:"architectures" json:"architectures"`
	DputHost      string   `yaml:"dput_host,omitempty" json:"dputHost,omitempty"` // default: ppa.launchpad.net
	SSHKeyEnv     string   `yaml:"ssh_key_env,omitempty" json:"sshKeyEnv,omitempty"`
	MirrorEnabled bool     `yaml:"mirror_enabled" json:"mirrorEnabled"`
	SubmitEnabled bool     `yaml:"submit_enabled" json:"submitEnabled"`
}

type DebianBuilddSpec struct {
	Name          string   `yaml:"name" json:"name"`
	BaseURL       string   `yaml:"base_url" json:"baseUrl"` // downstream mirror URL or mini-buildd archive
	Suites        []string `yaml:"suites" json:"suites"`    // e.g. bookworm, trixie
	Components    []string `yaml:"components" json:"components"`
	Architectures []string `yaml:"architectures" json:"architectures"`
	UploadHost    string   `yaml:"upload_host,omitempty" json:"uploadHost,omitempty"` // e.g. ftp.upload.debian.org / mentors.debian.net
	UploadMethod  string   `yaml:"upload_method,omitempty" json:"uploadMethod,omitempty"`
	MirrorEnabled bool     `yaml:"mirror_enabled" json:"mirrorEnabled"`
	SubmitEnabled bool     `yaml:"submit_enabled" json:"submitEnabled"`
}

type RPMUpstreamConfig struct {
	SigningKeyEnv string         `yaml:"signing_key_env,omitempty" json:"signingKeyEnv,omitempty"`
	OBS           []OBSSourceSpec `yaml:"obs,omitempty" json:"obs,omitempty"`
}

type OBSSourceSpec struct {
	Name          string   `yaml:"name" json:"name"`
	APIBaseURL    string   `yaml:"api_base_url,omitempty" json:"apiBaseUrl,omitempty"`         // default: https://api.opensuse.org
	DownloadBase  string   `yaml:"download_base,omitempty" json:"downloadBase,omitempty"`       // default: https://download.opensuse.org/repositories
	Project       string   `yaml:"project" json:"project"`                                      // e.g. home:svc-plus:release
	Package       string   `yaml:"package,omitempty" json:"package,omitempty"`
	Repositories  []string `yaml:"repositories,omitempty" json:"repositories,omitempty"`        // e.g. Fedora_41, openSUSE_Tumbleweed, RHEL_9
	Architectures []string `yaml:"architectures,omitempty" json:"architectures,omitempty"`      // x86_64, aarch64
	UserEnv       string   `yaml:"user_env,omitempty" json:"userEnv,omitempty"`
	PasswordEnv   string   `yaml:"password_env,omitempty" json:"passwordEnv,omitempty"`
	MirrorEnabled bool     `yaml:"mirror_enabled" json:"mirrorEnabled"`
	SubmitEnabled bool     `yaml:"submit_enabled" json:"submitEnabled"`
}

type WinGetUpstreamConfig struct {
	SourceIdentifier string `yaml:"source_identifier" json:"sourceIdentifier"` // e.g. SvcPlus.Repo
	RESTBasePath     string `yaml:"rest_base_path" json:"restBasePath"`       // default: /winget/api
	UpstreamPRRepo   string `yaml:"upstream_pr_repo,omitempty" json:"upstreamPrRepo,omitempty"`
}

type ProductSpec struct {
	ID                 string         `yaml:"id" json:"id"`
	Name               string         `yaml:"name" json:"name"`
	Aliases            []string       `yaml:"aliases,omitempty" json:"aliases,omitempty"`
	Category           string         `yaml:"category" json:"category"`
	Repo               string         `yaml:"repo" json:"repo"`
	Homepage           string         `yaml:"homepage" json:"homepage"`
	Summary            string         `yaml:"summary" json:"summary"`
	Publisher          string         `yaml:"publisher" json:"publisher"`
	License            string         `yaml:"license" json:"license"`
	DefaultChannel     string         `yaml:"default_channel" json:"defaultChannel"`
	Channels           []string       `yaml:"channels" json:"channels"`
	Platforms          []string       `yaml:"platforms" json:"platforms"`
	ReleaseMatrixURL   string         `yaml:"release_matrix_url" json:"releaseMatrixUrl"`
	CNReleaseMatrixURL string         `yaml:"cn_release_matrix_url" json:"cnReleaseMatrixUrl"`
	Packages           map[string]any `yaml:"packages,omitempty" json:"packages,omitempty"`
}

// LoadFiles merges one or more YAML config files (e.g. products.yaml, mirrors.yaml, upstreams.yaml).
func LoadFiles(paths ...string) (*Config, error) {
	cfg := DefaultConfig()
	for _, p := range paths {
		if strings.TrimSpace(p) == "" {
			continue
		}
		raw, err := os.ReadFile(p)
		if err != nil {
			return nil, fmt.Errorf("read config %s: %w", p, err)
		}
		if err := MergeYAML(cfg, raw); err != nil {
			return nil, fmt.Errorf("parse config %s: %w", p, err)
		}
	}
	return cfg, nil
}

// MergeYAML unmarshals YAML bytes into an existing Config.
func MergeYAML(cfg *Config, data []byte) error {
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return err
	}
	if cfg.Upstreams.OCI.RegistryAliases == nil {
		cfg.Upstreams.OCI.RegistryAliases = DefaultRegistryAliases()
	}
	if cfg.Upstreams.NPM.UpstreamRegistry == "" {
		cfg.Upstreams.NPM.UpstreamRegistry = "https://registry.npmjs.org"
	}
	if cfg.Upstreams.WinGet.SourceIdentifier == "" {
		cfg.Upstreams.WinGet.SourceIdentifier = "SvcPlus.Distribution"
	}
	if cfg.Upstreams.WinGet.RESTBasePath == "" {
		cfg.Upstreams.WinGet.RESTBasePath = "/winget/api"
	}
	return nil
}

func DefaultRegistryAliases() map[string]string {
	return map[string]string{
		"ghcr":   "ghcr.io",
		"docker": "registry-1.docker.io",
		"quay":   "quay.io",
		"k8s":    "registry.k8s.io",
	}
}

func DefaultConfig() *Config {
	return &Config{
		SchemaVersion: 2,
		MirrorName:    "global",
		PublicURL:     "https://repo.svc.plus",
		DLURL:         "https://dl.svc.plus",
		CDNRedirect:   true,
		Mirrors: MirrorsConfig{
			DL:     "https://dl.svc.plus",
			CNDL:   "https://cn-dl.svc.plus",
			Repo:   "https://repo.svc.plus",
			CNRepo: "https://cn-repo.svc.plus",
			GitHub: "https://github.com",
		},
		Storage: StorageConfig{
			Default: "local",
			Targets: map[string]StorageTargetSpec{},
		},
		Upstreams: UpstreamsConfig{
			NPM: NPMUpstreamConfig{
				UpstreamRegistry: "https://registry.npmjs.org",
				PullThrough:      true,
			},
			OCI: OCIUpstreamConfig{
				Mode:               "hybrid",
				PullThroughEnabled: true,
				RegistryAliases:    DefaultRegistryAliases(),
			},
			Helm: HelmUpstreamConfig{
				Mode: "dual",
			},
			WinGet: WinGetUpstreamConfig{
				SourceIdentifier: "SvcPlus.Distribution",
				RESTBasePath:     "/winget/api",
			},
		},
		Products: map[string]ProductSpec{},
	}
}
