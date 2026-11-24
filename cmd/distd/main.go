//go:build !js

package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ai-workspace-lab/distribution/internal/config"
	"github.com/ai-workspace-lab/distribution/internal/crypto"
	"github.com/ai-workspace-lab/distribution/internal/server"
	"github.com/ai-workspace-lab/distribution/internal/storage"
	"github.com/ai-workspace-lab/distribution/internal/syncer"
)

const version = "2.0.0"

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}
	sub := os.Args[1]
	args := os.Args[2:]
	switch sub {
	case "serve":
		if err := runServe(args); err != nil {
			log.Fatalf("serve failed: %v", err)
		}
	case "sync":
		if err := runSync(args); err != nil {
			log.Fatalf("sync failed: %v", err)
		}
	case "submit":
		if err := runSubmit(args); err != nil {
			log.Fatalf("submit failed: %v", err)
		}
	case "index":
		if err := runIndex(args); err != nil {
			log.Fatalf("index failed: %v", err)
		}
	case "version", "-v", "--version":
		fmt.Printf("distd %s (unified Go distribution hub: files/https/rsync, npm, github/ghcr, docker/oci, helm, apt/ppa/debian, dnf/obs, winget)\n", version)
	case "-h", "--help", "help":
		printUsage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n", sub)
		printUsage()
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Println(`distd — Unified Lightweight Go Distribution Hub (CDN-friendly, Cloudflare Workers + Local Single Binary)

Usage:
  distd serve   [flags]   Start unified HTTP/HTTPS distribution server (files, npm, github, docker/oci, helm, apt, rpm, winget)
  distd sync    [flags]   Aggregate and sync upstreams into configured storage targets (R2 / Aliyun OSS / Harbor / Local)
  distd submit  [flags]   Submit upstream source packages to Ubuntu PPA, Debian buildd, or openSUSE OBS
  distd index   [flags]   Build pure-Go signed APT/RPM/WinGet repository indexes from local or remote pools
  distd version           Show version and supported protocols`)
}

func loadMergedConfig(configFiles string) (*config.Config, error) {
	if strings.TrimSpace(configFiles) == "" {
		return config.DefaultConfig(), nil
	}
	parts := strings.Split(configFiles, ",")
	var existing []string
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if _, err := os.Stat(p); err == nil {
			existing = append(existing, p)
		}
	}
	if len(existing) == 0 {
		return config.DefaultConfig(), nil
	}
	return config.LoadFiles(existing...)
}

func resolveStorage(cfg *config.Config, targetCSV, localDir string) (storage.Backend, error) {
	if localDir != "" {
		return storage.NewLocalFSBackend(localDir, "")
	}
	if targetCSV == "" {
		targetCSV = cfg.Storage.Default
	}
	names := strings.Split(targetCSV, ",")
	var backends []storage.Backend
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if spec, ok := cfg.Storage.Targets[name]; ok {
			b, err := storage.FromSpec(name, spec)
			if err != nil {
				return nil, err
			}
			backends = append(backends, b)
		} else if name == "memory" {
			backends = append(backends, storage.NewMemoryBackend(""))
		} else if name == "local" {
			b, err := storage.NewLocalFSBackend("./dist-data", "")
			if err != nil {
				return nil, err
			}
			backends = append(backends, b)
		} else {
			return nil, fmt.Errorf("unknown storage target %q", name)
		}
	}
	if len(backends) == 0 {
		return storage.NewLocalFSBackend("./dist-data", "")
	}
	if len(backends) == 1 {
		return backends[0], nil
	}
	return storage.NewMultiBackend(backends...)
}

func loadOptionalSigner(envKey, passEnv string) (*crypto.Signer, error) {
	if envKey == "" {
		envKey = "GPG_PRIVATE_KEY"
	}
	armored := os.Getenv(envKey)
	if strings.TrimSpace(armored) == "" {
		return nil, nil
	}
	pass := ""
	if passEnv != "" {
		pass = os.Getenv(passEnv)
	}
	return crypto.LoadArmoredPrivateKey(armored, pass)
}

func runServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	listen := fs.String("listen", ":8080", "HTTP listen address")
	cfgFiles := fs.String("config", "catalog/products.yaml,catalog/mirrors.yaml,catalog/upstreams.yaml", "Comma-separated YAML config files")
	storageTarget := fs.String("storage", "", "Storage target name(s) from mirrors.yaml")
	dataDir := fs.String("data-dir", "", "Override local filesystem storage directory")
	mirrorName := fs.String("mirror", "", "Mirror identity (global | cn | local)")
	publicURL := fs.String("public-url", "", "Public base URL for rewritten npm/helm/winget links")
	harborURL := fs.String("harbor-url", "", "Optional Harbor upstream URL for OCI/Helm proxy")
	cdnRedirect := fs.Bool("cdn-redirect", true, "Return 302 CDN redirects for large immutable blobs when public URL is configured")
	_ = fs.Parse(args)

	cfg, err := loadMergedConfig(*cfgFiles)
	if err != nil {
		return err
	}
	if *mirrorName != "" {
		cfg.MirrorName = *mirrorName
	}
	if *publicURL != "" {
		cfg.PublicURL = *publicURL
	}
	if *harborURL != "" {
		cfg.Upstreams.OCI.HarborURL = *harborURL
	}
	cfg.CDNRedirect = *cdnRedirect

	store, err := resolveStorage(cfg, *storageTarget, *dataDir)
	if err != nil {
		return err
	}
	h := server.NewHandler(cfg, store, nil)
	srv := &http.Server{
		Addr:              *listen,
		Handler:           h,
		ReadHeaderTimeout: 15 * time.Second,
	}
	log.Printf("distd %s listening on %s (mirror=%s storage=%s cdnRedirect=%v)", version, *listen, cfg.MirrorName, store.Name(), cfg.CDNRedirect)
	return srv.ListenAndServe()
}

func runSync(args []string) error {
	fs := flag.NewFlagSet("sync", flag.ExitOnError)
	cfgFiles := fs.String("config", "catalog/products.yaml,catalog/mirrors.yaml,catalog/upstreams.yaml", "Comma-separated YAML config files")
	storageTarget := fs.String("storage", "", "Comma-separated storage targets (e.g. r2,oss or local)")
	dataDir := fs.String("data-dir", "", "Override local filesystem storage directory")
	only := fs.String("only", "all", "Component to sync: all|files|npm|github|oci|helm|apt|rpm|winget")
	_ = fs.Parse(args)

	cfg, err := loadMergedConfig(*cfgFiles)
	if err != nil {
		return err
	}
	store, err := resolveStorage(cfg, *storageTarget, *dataDir)
	if err != nil {
		return err
	}
	signer, err := loadOptionalSigner("GPG_PRIVATE_KEY", "GPG_PASSPHRASE")
	if err != nil {
		return err
	}

	ctx := context.Background()
	filesSyncer := syncer.NewFileSyncer(store, nil)
	ociSyncer := syncer.NewOCISyncer(store, cfg.Upstreams.OCI, nil)

	want := func(name string) bool {
		return *only == "all" || strings.EqualFold(*only, name)
	}

	if want("files") {
		for _, spec := range cfg.Upstreams.Files {
			n, err := filesSyncer.SyncSpec(ctx, spec)
			if err != nil {
				return fmt.Errorf("sync files %s: %w", spec.Name, err)
			}
			log.Printf("[files] %s synced %d object(s)", spec.Name, n)
		}
	}

	if want("npm") {
		npmSyncer := syncer.NewNPMSyncer(store, cfg.Upstreams.NPM.UpstreamRegistry, strings.TrimRight(cfg.PublicURL, "/")+"/npm", nil)
		for _, pkg := range cfg.Upstreams.NPM.Packages {
			n, err := npmSyncer.SyncPackage(ctx, pkg, false)
			if err != nil {
				return fmt.Errorf("sync npm %s: %w", pkg, err)
			}
			log.Printf("[npm] %s synced %d tarball(s)", pkg, n)
		}
	}

	if want("github") {
		ghSyncer := syncer.NewGitHubSyncer(store, filesSyncer, ociSyncer, cfg.Upstreams.GitHub, cfg.Mirrors.GitHub)
		for _, repo := range cfg.Upstreams.GitHub.Repos {
			n, err := ghSyncer.SyncRepo(ctx, repo)
			if err != nil {
				return fmt.Errorf("sync github %s: %w", repo.Repo, err)
			}
			log.Printf("[github] %s synced %d object(s)", repo.Repo, n)
		}
	}

	if want("oci") {
		for _, img := range cfg.Upstreams.OCI.Mirrors {
			tags := img.Tags
			if len(tags) == 0 {
				tags = []string{"latest"}
			}
			for _, tag := range tags {
				n, err := ociSyncer.SyncImage(ctx, img, tag)
				if err != nil {
					return fmt.Errorf("sync oci %s:%s: %w", img.Source, tag, err)
				}
				log.Printf("[oci] %s:%s synced %d blob(s)", img.Source, tag, n)
			}
		}
	}

	if want("helm") {
		helmSyncer := syncer.NewHelmSyncer(store, ociSyncer, strings.TrimRight(cfg.PublicURL, "/")+"/helm", nil)
		n, err := helmSyncer.SyncAll(ctx, cfg.Upstreams.Helm)
		if err != nil {
			return fmt.Errorf("sync helm: %w", err)
		}
		log.Printf("[helm] synced %d chart/blob(s)", n)
	}

	if want("apt") {
		ppaSyncer := syncer.NewDebianPPASyncer(store, signer, nil)
		var allDebs []syncer.DebPackage
		for _, ppa := range cfg.Upstreams.APT.PPAs {
			debs, err := ppaSyncer.MirrorUbuntuPPA(ctx, ppa)
			if err != nil {
				return fmt.Errorf("mirror ppa %s: %w", ppa.Name, err)
			}
			allDebs = append(allDebs, debs...)
		}
		for _, debRepo := range cfg.Upstreams.APT.DebianRepos {
			debs, err := ppaSyncer.MirrorDebianBuildd(ctx, debRepo)
			if err != nil {
				return fmt.Errorf("mirror debian %s: %w", debRepo.Name, err)
			}
			allDebs = append(allDebs, debs...)
		}
		if len(allDebs) > 0 {
			aptBuilder := syncer.NewAPTBuilder(store, signer)
			if err := aptBuilder.PublishSuite(ctx, "stable", "main", cfg.Upstreams.APT.Architectures, allDebs); err != nil {
				return fmt.Errorf("publish apt suite: %w", err)
			}
			log.Printf("[apt] published %d deb package(s)", len(allDebs))
		}
	}

	if want("rpm") {
		var allRPMs []syncer.RPMPackage
		for _, obsSpec := range cfg.Upstreams.RPM.OBS {
			client := syncer.NewOBSClient(store, obsSpec, nil)
			rpms, err := client.MirrorOBSRPMs(ctx, obsSpec)
			if err != nil {
				return fmt.Errorf("mirror obs %s: %w", obsSpec.Name, err)
			}
			allRPMs = append(allRPMs, rpms...)
		}
		if len(allRPMs) > 0 {
			rpmBuilder := syncer.NewRPMBuilder(store, signer)
			byArch := map[string][]syncer.RPMPackage{}
			for _, p := range allRPMs {
				byArch[p.Arch] = append(byArch[p.Arch], p)
			}
			for arch, list := range byArch {
				if err := rpmBuilder.PublishRepo(ctx, "stable", arch, list); err != nil {
					return fmt.Errorf("publish rpm %s: %w", arch, err)
				}
			}
			log.Printf("[rpm] published %d rpm package(s)", len(allRPMs))
		}
	}

	return nil
}

func runSubmit(args []string) error {
	fs := flag.NewFlagSet("submit", flag.ExitOnError)
	target := fs.String("target", "", "Upstream build target: ppa | debian | obs")
	project := fs.String("project", "", "OBS project or Launchpad PPA owner/ppa")
	pkg := fs.String("package", "", "OBS package name")
	sourceDir := fs.String("dir", ".", "Directory containing .changes (ppa/debian) or .spec/tarball (obs)")
	host := fs.String("host", "", "Override upload host (e.g. ppa.launchpad.net, mentors.debian.net, https://api.opensuse.org)")
	dryRun := fs.Bool("dry-run", false, "Preview upstream submission without uploading")
	_ = fs.Parse(args)

	signer, err := loadOptionalSigner("GPG_PRIVATE_KEY", "GPG_PASSPHRASE")
	if err != nil {
		return err
	}
	ctx := context.Background()

	switch strings.ToLower(*target) {
	case "ppa", "debian":
		matches, _ := filepath.Glob(filepath.Join(*sourceDir, "*.changes"))
		if len(matches) == 0 {
			return fmt.Errorf("no *.changes file found in %s", *sourceDir)
		}
		uploadHost := *host
		incoming := "/"
		if *target == "ppa" {
			if uploadHost == "" {
				uploadHost = "ppa.launchpad.net"
			}
			incoming = fmt.Sprintf("~%s/ubuntu/", strings.TrimPrefix(*project, "~"))
		} else if uploadHost == "" {
			uploadHost = "mentors.debian.net"
		}
		s := syncer.NewDebianPPASyncer(nil, signer, nil)
		out, err := s.SubmitSourceChanges(ctx, matches[0], uploadHost, incoming, "ftp", *dryRun)
		fmt.Println(out)
		return err

	case "obs":
		if *project == "" || *pkg == "" {
			return fmt.Errorf("--project and --package are required for OBS submit")
		}
		spec := config.OBSSourceSpec{
			APIBaseURL:  *host,
			Project:     *project,
			Package:     *pkg,
			UserEnv:     "OBS_USER",
			PasswordEnv: "OBS_PASSWORD",
		}
		client := syncer.NewOBSClient(nil, spec, nil)
		entries, err := os.ReadDir(*sourceDir)
		if err != nil {
			return err
		}
		for _, ent := range entries {
			if ent.IsDir() {
				continue
			}
			if *dryRun {
				fmt.Printf("[dry-run] OBS PUT /source/%s/%s/%s\n", *project, *pkg, ent.Name())
				continue
			}
			raw, err := os.ReadFile(filepath.Join(*sourceDir, ent.Name()))
			if err != nil {
				return err
			}
			if err := client.PutSourceFile(ctx, *project, *pkg, ent.Name(), raw); err != nil {
				return err
			}
		}
		if *dryRun {
			fmt.Printf("[dry-run] OBS POST /build/%s?cmd=rebuild&package=%s\n", *project, *pkg)
			return nil
		}
		return client.TriggerRebuild(ctx, *project, *pkg)

	default:
		return fmt.Errorf("unsupported --target %q (expected ppa | debian | obs)", *target)
	}
}

func runIndex(args []string) error {
	fs := flag.NewFlagSet("index", flag.ExitOnError)
	dataDir := fs.String("data-dir", "./dist-data", "Local storage root directory containing apt/pool and rpm/packages")
	suite := fs.String("suite", "stable", "APT suite / RPM channel")
	_ = fs.Parse(args)

	store, err := storage.NewLocalFSBackend(*dataDir, "")
	if err != nil {
		return err
	}
	signer, err := loadOptionalSigner("GPG_PRIVATE_KEY", "GPG_PASSPHRASE")
	if err != nil {
		return err
	}
	ctx := context.Background()

	// Scan local .deb files
	debItems, _ := store.List(ctx, "apt/pool")
	var debs []syncer.DebPackage
	for _, it := range debItems {
		if !strings.HasSuffix(it.Key, ".deb") {
			continue
		}
		obj, err := store.Get(ctx, it.Key)
		if err != nil {
			continue
		}
		raw, _ := os.ReadFile(filepath.Join(*dataDir, filepath.FromSlash(it.Key)))
		_ = obj.Body.Close()
		pkg, err := syncer.ParseDebControl(raw, strings.TrimPrefix(it.Key, "apt/"))
		if err == nil {
			debs = append(debs, pkg)
		}
	}
	if len(debs) > 0 {
		if err := syncer.NewAPTBuilder(store, signer).PublishSuite(ctx, *suite, "main", []string{"amd64", "arm64"}, debs); err != nil {
			return err
		}
		log.Printf("[index] built APT suite %s (%d .deb)", *suite, len(debs))
	}

	// Scan local .rpm files
	rpmItems, _ := store.List(ctx, "rpm/packages")
	byArch := map[string][]syncer.RPMPackage{}
	for _, it := range rpmItems {
		if !strings.HasSuffix(it.Key, ".rpm") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(*dataDir, filepath.FromSlash(it.Key)))
		if err != nil {
			continue
		}
		pkg, err := syncer.ParseRPMHeader(raw, strings.TrimPrefix(it.Key, "rpm/"))
		if err == nil {
			byArch[pkg.Arch] = append(byArch[pkg.Arch], pkg)
		}
	}
	for arch, list := range byArch {
		if err := syncer.NewRPMBuilder(store, signer).PublishRepo(ctx, *suite, arch, list); err != nil {
			return err
		}
		log.Printf("[index] built RPM repo %s/%s (%d .rpm)", *suite, arch, len(list))
	}
	return nil
}
