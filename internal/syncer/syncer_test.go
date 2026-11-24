//go:build !js

package syncer

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ai-workspace-lab/distribution/internal/config"
	"github.com/ai-workspace-lab/distribution/internal/crypto"
	"github.com/ai-workspace-lab/distribution/internal/server"
	"github.com/ai-workspace-lab/distribution/internal/storage"
)

func buildSyntheticDeb(t *testing.T, pkg, ver, arch string) []byte {
	t.Helper()
	var tarBuf bytes.Buffer
	gzw := gzip.NewWriter(&tarBuf)
	tw := tar.NewWriter(gzw)
	control := fmt.Sprintf("Package: %s\nVersion: %s\nArchitecture: %s\nMaintainer: svc.plus\nDescription: Synthetic deb package\n", pkg, ver, arch)
	_ = tw.WriteHeader(&tar.Header{Name: "./control", Mode: 0o644, Size: int64(len(control))})
	_, _ = tw.Write([]byte(control))
	_ = tw.Close()
	_ = gzw.Close()
	ctrlBytes := tarBuf.Bytes()

	var ar bytes.Buffer
	ar.WriteString("!<arch>\n")
	writeArMember := func(name string, payload []byte) {
		hdr := fmt.Sprintf("%-16s%-12d%-6d%-6d%-8o%-10d`\n", name, 0, 0, 0, 0o100644, len(payload))
		ar.WriteString(hdr)
		ar.Write(payload)
		if len(payload)%2 == 1 {
			ar.WriteByte('\n')
		}
	}
	writeArMember("debian-binary", []byte("2.0\n"))
	writeArMember("control.tar.gz", ctrlBytes)
	return ar.Bytes()
}

func buildSyntheticRPM(name, ver, rel, arch string) []byte {
	var buf bytes.Buffer
	// 96-byte lead
	lead := make([]byte, 96)
	lead[0], lead[1], lead[2], lead[3] = 0xed, 0xab, 0xee, 0xdb
	buf.Write(lead)
	// Empty signature header (16 bytes, aligned to 8)
	sig := make([]byte, 16)
	sig[0], sig[1], sig[2], sig[3] = 0x8e, 0xad, 0xe8, 0x01
	buf.Write(sig)

	// Main header with 4 string tags: 1000=Name, 1001=Version, 1002=Release, 1022=Arch
	var store bytes.Buffer
	type entry struct {
		tag uint32
		off uint32
	}
	addStr := func(tag uint32, val string) entry {
		off := uint32(store.Len())
		store.WriteString(val)
		store.WriteByte(0)
		return entry{tag: tag, off: off}
	}
	entries := []entry{
		addStr(1000, name),
		addStr(1001, ver),
		addStr(1002, rel),
		addStr(1022, arch),
	}
	mainHdr := make([]byte, 16)
	mainHdr[0], mainHdr[1], mainHdr[2], mainHdr[3] = 0x8e, 0xad, 0xe8, 0x01
	binary.BigEndian.PutUint32(mainHdr[8:12], uint32(len(entries)))
	binary.BigEndian.PutUint32(mainHdr[12:16], uint32(store.Len()))
	buf.Write(mainHdr)
	for _, e := range entries {
		rec := make([]byte, 16)
		binary.BigEndian.PutUint32(rec[0:4], e.tag)
		binary.BigEndian.PutUint32(rec[4:8], 6) // STRING
		binary.BigEndian.PutUint32(rec[8:12], e.off)
		binary.BigEndian.PutUint32(rec[12:16], 1)
		buf.Write(rec)
	}
	buf.Write(store.Bytes())
	return buf.Bytes()
}

func TestPureGoAPTAndRPMBuildersWithOpenPGP(t *testing.T) {
	ctx := context.Background()
	store := storage.NewMemoryBackend("https://repo.svc.plus")
	signer, err := crypto.GenerateEphemeralKey("svc.plus Release", "test", "release@svc.plus")
	if err != nil {
		t.Fatalf("GenerateEphemeralKey: %v", err)
	}

	// Pure-Go .deb parsing & signed APT suite
	debBytes := buildSyntheticDeb(t, "xconnect-one", "0.1.22", "amd64")
	debPkg, err := ParseDebControl(debBytes, "pool/main/x/xconnect-one_0.1.22_amd64.deb")
	if err != nil {
		t.Fatalf("ParseDebControl: %v", err)
	}
	if debPkg.Package != "xconnect-one" || debPkg.Version != "0.1.22" || debPkg.Architecture != "amd64" {
		t.Fatalf("unexpected debPkg: %+v", debPkg)
	}
	if err := NewAPTBuilder(store, signer).PublishSuite(ctx, "stable", "main", []string{"amd64", "arm64"}, []DebPackage{debPkg}); err != nil {
		t.Fatalf("PublishSuite: %v", err)
	}
	for _, key := range []string{
		"apt/dists/stable/main/binary-amd64/Packages",
		"apt/dists/stable/main/binary-amd64/Packages.gz",
		"apt/dists/stable/Release",
		"apt/dists/stable/InRelease",
		"apt/dists/stable/Release.gpg",
		"apt/svc-plus-archive-keyring.gpg",
	} {
		obj, err := store.Get(ctx, key)
		if err != nil {
			t.Fatalf("missing APT artifact %s: %v", key, err)
		}
		b, _ := io.ReadAll(obj.Body)
		_ = obj.Body.Close()
		if key == "apt/dists/stable/InRelease" && !strings.Contains(string(b), "-----BEGIN PGP SIGNED MESSAGE-----") {
			t.Fatalf("InRelease missing PGP clearsign header")
		}
	}

	// Pure-Go .rpm parsing & signed RPM repodata
	rpmBytes := buildSyntheticRPM("xconnect-one", "0.1.22", "1", "x86_64")
	rpmPkg, err := ParseRPMHeader(rpmBytes, "packages/xconnect-one-0.1.22-1.x86_64.rpm")
	if err != nil {
		t.Fatalf("ParseRPMHeader: %v", err)
	}
	if rpmPkg.Name != "xconnect-one" || rpmPkg.Version != "0.1.22" || rpmPkg.Arch != "x86_64" {
		t.Fatalf("unexpected rpmPkg: %+v", rpmPkg)
	}
	if err := NewRPMBuilder(store, signer).PublishRepo(ctx, "stable", "x86_64", []RPMPackage{rpmPkg}); err != nil {
		t.Fatalf("PublishRepo: %v", err)
	}
	for _, key := range []string{
		"rpm/stable/x86_64/repodata/repomd.xml",
		"rpm/stable/x86_64/repodata/repomd.xml.asc",
		"rpm/stable/x86_64/repodata/primary.xml.gz",
		"rpm/RPM-GPG-KEY-svc-plus",
	} {
		if _, err := store.Head(ctx, key); err != nil {
			t.Fatalf("missing RPM artifact %s: %v", key, err)
		}
	}
}

func TestOBSClientAndDebianPPASyncer(t *testing.T) {
	ctx := context.Background()
	store := storage.NewMemoryBackend("")
	var uploadedSpec string
	var rebuilt bool

	// Mock openSUSE OBS API & download server
	obsSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPut && r.URL.Path == "/source/home:svc-plus/xconnect/xconnect.spec":
			b, _ := io.ReadAll(r.Body)
			uploadedSpec = string(b)
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPost && r.URL.Path == "/build/home:svc-plus" && r.URL.Query().Get("cmd") == "rebuild":
			rebuilt = true
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodGet && r.URL.Path == "/build/home:svc-plus/_result":
			_, _ = w.Write([]byte(`<resultlist><result repository="Fedora_41" arch="x86_64" state="published"><status package="xconnect" code="succeeded"/></result></resultlist>`))
		case r.URL.Path == "/repositories/home:/svc-plus/Fedora_41/repodata/repomd.xml":
			_, _ = w.Write([]byte(`<repomd><data type="primary"><location href="repodata/primary.xml"/></data></repomd>`))
		case r.URL.Path == "/repositories/home:/svc-plus/Fedora_41/repodata/primary.xml":
			_, _ = w.Write([]byte(`<metadata><package><name>xconnect</name><arch>x86_64</arch><version epoch="0" ver="1.1.9" rel="1"/><location href="x86_64/xconnect-1.1.9-1.x86_64.rpm"/></package></metadata>`))
		case r.URL.Path == "/repositories/home:/svc-plus/Fedora_41/x86_64/xconnect-1.1.9-1.x86_64.rpm":
			_, _ = w.Write(buildSyntheticRPM("xconnect", "1.1.9", "1", "x86_64"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer obsSrv.Close()

	spec := config.OBSSourceSpec{
		Name:          "test-obs",
		APIBaseURL:    obsSrv.URL,
		DownloadBase:  obsSrv.URL + "/repositories",
		Project:       "home:svc-plus",
		Repositories:  []string{"Fedora_41"},
		Architectures: []string{"x86_64"},
		MirrorEnabled: true,
	}
	obs := NewOBSClient(store, spec, obsSrv.Client())
	if err := obs.PutSourceFile(ctx, "home:svc-plus", "xconnect", "xconnect.spec", []byte("Name: xconnect\n")); err != nil {
		t.Fatalf("OBS PutSourceFile: %v", err)
	}
	if uploadedSpec != "Name: xconnect\n" {
		t.Fatalf("unexpected uploaded spec: %q", uploadedSpec)
	}
	if err := obs.TriggerRebuild(ctx, "home:svc-plus", "xconnect"); err != nil || !rebuilt {
		t.Fatalf("OBS TriggerRebuild failed: err=%v rebuilt=%v", err, rebuilt)
	}
	results, err := obs.GetBuildResults(ctx, "home:svc-plus", "xconnect")
	if err != nil || len(results) != 1 || results[0].Repository != "Fedora_41" {
		t.Fatalf("OBS GetBuildResults: %+v err=%v", results, err)
	}
	rpms, err := obs.MirrorOBSRPMs(ctx, spec)
	if err != nil || len(rpms) != 1 || rpms[0].Name != "xconnect" {
		t.Fatalf("OBS MirrorOBSRPMs: %+v err=%v", rpms, err)
	}

	// Mock Debian buildd / PPA downstream mirror + upstream dput dry-run
	debBytes := buildSyntheticDeb(t, "xconnect", "1.1.9", "amd64")
	aptSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/dists/bookworm/main/binary-amd64/Packages":
			_, _ = w.Write([]byte("Package: xconnect\nVersion: 1.1.9\nArchitecture: amd64\nFilename: pool/main/x/xconnect_1.1.9_amd64.deb\n\n"))
		case "/pool/main/x/xconnect_1.1.9_amd64.deb":
			_, _ = w.Write(debBytes)
		default:
			http.NotFound(w, r)
		}
	}))
	defer aptSrv.Close()

	signer, _ := crypto.GenerateEphemeralKey("svc.plus", "test", "dev@svc.plus")
	ppa := NewDebianPPASyncer(store, signer, aptSrv.Client())
	debs, err := ppa.MirrorDebianBuildd(ctx, config.DebianBuilddSpec{
		Name:          "debian-test",
		BaseURL:       aptSrv.URL,
		Suites:        []string{"bookworm"},
		Components:    []string{"main"},
		Architectures: []string{"amd64"},
		MirrorEnabled: true,
	})
	if err != nil || len(debs) != 1 || debs[0].Package != "xconnect" {
		t.Fatalf("MirrorDebianBuildd: %+v err=%v", debs, err)
	}

	changesFile := filepath.Join(t.TempDir(), "xconnect_1.1.9_source.changes")
	_ = os.WriteFile(changesFile, []byte("Format: 1.8\nSource: xconnect\n"), 0o644)
	out, err := ppa.SubmitSourceChanges(ctx, changesFile, "ppa.launchpad.net", "~svc-plus/ubuntu/", "ftp", true)
	if err != nil || !strings.Contains(out, "[dry-run]") {
		t.Fatalf("SubmitSourceChanges dry-run: err=%v out=%q", err, out)
	}
	signedChanges, _ := os.ReadFile(changesFile)
	if !strings.Contains(string(signedChanges), "-----BEGIN PGP SIGNED MESSAGE-----") {
		t.Fatalf("expected .changes file to be OpenPGP clearsigned")
	}
}

func TestOCISyncerStandaloneAndHarborAndHelmAndWinGet(t *testing.T) {
	ctx := context.Background()
	store := storage.NewMemoryBackend("https://repo.svc.plus")

	layerData := []byte("hello-oci-layer")
	layerSum := sha256.Sum256(layerData)
	layerDigest := "sha256:" + hex.EncodeToString(layerSum[:])
	manifestData := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","layers":[{"mediaType":"application/vnd.oci.image.layer.v1.tar+gzip","digest":"` + layerDigest + `","size":15}]}`)

	// Mock source registry (e.g. GHCR) + target Harbor registry
	harborPushedManifest := false
	ociSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v2/org/app/manifests/v1.0.0":
			w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
			_, _ = w.Write(manifestData)
		case r.Method == http.MethodGet && r.URL.Path == "/v2/org/app/blobs/"+layerDigest:
			_, _ = w.Write(layerData)
		case r.Method == http.MethodHead && r.URL.Path == "/v2/svc-plus/app/blobs/"+layerDigest:
			http.NotFound(w, r)
		case r.Method == http.MethodPost && r.URL.Path == "/v2/svc-plus/app/blobs/uploads/":
			w.Header().Set("Location", "/v2/svc-plus/app/blobs/uploads/uuid-1")
			w.WriteHeader(http.StatusAccepted)
		case r.Method == http.MethodPut && r.URL.Path == "/v2/svc-plus/app/blobs/uploads/uuid-1":
			w.WriteHeader(http.StatusCreated)
		case r.Method == http.MethodPut && r.URL.Path == "/v2/svc-plus/app/manifests/v1.0.0":
			harborPushedManifest = true
			w.WriteHeader(http.StatusCreated)
		default:
			http.NotFound(w, r)
		}
	}))
	defer ociSrv.Close()

	ociSyncer := NewOCISyncer(store, config.OCIUpstreamConfig{
		Mode:      "hybrid",
		HarborURL: ociSrv.URL,
	}, ociSrv.Client())
	n, err := ociSyncer.syncManifestRecursive(ctx, ociSrv.URL, "org/app", "svc-plus/app", "v1.0.0", true, true)
	if err != nil || n != 1 || !harborPushedManifest {
		t.Fatalf("OCI hybrid sync failed: n=%d harborPushed=%v err=%v", n, harborPushedManifest, err)
	}
	if _, err := store.Head(ctx, "oci/blobs/"+layerDigest); err != nil {
		t.Fatalf("expected OCI blob in R2/storage: %v", err)
	}

	// Helm classic repo sync
	helmSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/index.yaml":
			_, _ = w.Write([]byte("apiVersion: v1\nentries:\n  demo:\n    - name: demo\n      version: 0.2.0\n      urls:\n        - demo-0.2.0.tgz\n"))
		case "/demo-0.2.0.tgz":
			_, _ = w.Write([]byte("fake-chart"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer helmSrv.Close()

	hs := NewHelmSyncer(store, nil, "https://repo.svc.plus/helm", helmSrv.Client())
	hn, err := hs.SyncAll(ctx, config.HelmUpstreamConfig{
		Repos: []config.HelmRepoMirror{{Name: "test", URL: helmSrv.URL}},
	})
	if err != nil || hn != 1 {
		t.Fatalf("Helm SyncAll failed: n=%d err=%v", hn, err)
	}
	if _, err := store.Head(ctx, "helm/index.yaml"); err != nil {
		t.Fatalf("missing helm/index.yaml: %v", err)
	}

	// WinGet YAML + REST index publish
	ws := NewWinGetSyncer(store, "SvcPlus.Distribution")
	if err := ws.Publish(ctx, []server.WinGetPackageRecord{
		{
			PackageIdentifier: "SvcPlus.XConnect",
			PackageName:       "XConnect",
			Publisher:         "svc.plus",
			Moniker:           "xconnect",
			Versions: []server.WinGetVersionManifest{
				{
					PackageVersion: "1.1.9",
					Installers: []server.WinGetInstaller{
						{Architecture: "x64", InstallerType: "msi", InstallerUrl: "https://dl.svc.plus/x.msi", InstallerSha256: strings.Repeat("a", 64)},
					},
				},
			},
		},
	}); err != nil {
		t.Fatalf("WinGet Publish: %v", err)
	}
	if _, err := store.Head(ctx, "winget/api/index.json"); err != nil {
		t.Fatalf("missing winget/api/index.json: %v", err)
	}
	if _, err := store.Head(ctx, "winget/manifests/s/SvcPlus/XConnect/1.1.9/SvcPlus.XConnect.yaml"); err != nil {
		t.Fatalf("missing static winget yaml manifest: %v", err)
	}
}
