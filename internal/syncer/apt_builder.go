//go:build !js

package syncer

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ai-workspace-lab/distribution/internal/crypto"
	"github.com/ai-workspace-lab/distribution/internal/storage"
)

// DebPackage represents parsed control metadata and storage location for a .deb package.
type DebPackage struct {
	Package      string
	Version      string
	Architecture string
	Maintainer   string
	Section      string
	Priority     string
	Depends      string
	Homepage     string
	Description  string
	Filename     string // relative to apt/ root, e.g. pool/main/x/xconnect/xconnect_1.1.9_amd64.deb
	Digests      crypto.Digests
}

// ParseDebControl extracts the Debian `control` stanza from a `.deb` (ar archive containing control.tar[.gz]).
// Pure Go implementation — requires no external `dpkg-deb` or `dpkg-scanpackages`.
func ParseDebControl(debBytes []byte, fallbackFilename string) (DebPackage, error) {
	dig := crypto.ComputeDigestsBytes(debBytes)
	pkg := DebPackage{
		Filename:     fallbackFilename,
		Digests:      dig,
		Section:      "utils",
		Priority:     "optional",
		Architecture: "amd64",
	}

	controlText, err := extractControlFromAr(debBytes)
	if err != nil {
		return pkg, err
	}
	fields := parseDebianStanza(controlText)
	if v := fields["Package"]; v != "" {
		pkg.Package = v
	}
	if v := fields["Version"]; v != "" {
		pkg.Version = v
	}
	if v := fields["Architecture"]; v != "" {
		pkg.Architecture = v
	}
	pkg.Maintainer = fields["Maintainer"]
	if v := fields["Section"]; v != "" {
		pkg.Section = v
	}
	if v := fields["Priority"]; v != "" {
		pkg.Priority = v
	}
	pkg.Depends = fields["Depends"]
	pkg.Homepage = fields["Homepage"]
	pkg.Description = fields["Description"]
	return pkg, nil
}

func extractControlFromAr(data []byte) (string, error) {
	const magic = "!<arch>\n"
	if len(data) < len(magic) || string(data[:len(magic)]) != magic {
		return "", fmt.Errorf("not a valid ar/.deb archive")
	}
	offset := len(magic)
	for offset+60 <= len(data) {
		hdr := data[offset : offset+60]
		offset += 60
		name := strings.TrimRight(string(hdr[0:16]), " /")
		sizeStr := strings.TrimSpace(string(hdr[48:58]))
		size, err := strconv.Atoi(sizeStr)
		if err != nil || size < 0 || offset+size > len(data) {
			return "", fmt.Errorf("invalid ar member header for %q", name)
		}
		member := data[offset : offset+size]
		offset += size
		if offset%2 == 1 {
			offset++
		}
		if name == "control.tar.gz" || name == "control.tar" {
			var rdr io.Reader = bytes.NewReader(member)
			if strings.HasSuffix(name, ".gz") {
				gz, err := gzip.NewReader(rdr)
				if err != nil {
					return "", err
				}
				defer gz.Close()
				rdr = gz
			}
			tr := tar.NewReader(rdr)
			for {
				th, err := tr.Next()
				if err == io.EOF {
					break
				}
				if err != nil {
					return "", err
				}
				clean := strings.TrimPrefix(path.Clean(th.Name), "./")
				if clean == "control" {
					b, err := io.ReadAll(tr)
					if err != nil {
						return "", err
					}
					return string(b), nil
				}
			}
		}
	}
	return "", fmt.Errorf("control file not found in .deb archive")
}

func parseDebianStanza(raw string) map[string]string {
	fields := map[string]string{}
	var lastKey string
	for _, line := range strings.Split(strings.ReplaceAll(raw, "\r\n", "\n"), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		if (strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t")) && lastKey != "" {
			fields[lastKey] += "\n" + line
			continue
		}
		if idx := strings.IndexByte(line, ':'); idx > 0 {
			k := strings.TrimSpace(line[:idx])
			v := strings.TrimSpace(line[idx+1:])
			fields[k] = v
			lastKey = k
		}
	}
	return fields
}

// APTBuilder builds complete signed APT repositories in pure Go (Packages, Packages.gz, Release, InRelease, Release.gpg).
type APTBuilder struct {
	Store  storage.Backend
	Signer *crypto.Signer
	Origin string
	Label  string
}

func NewAPTBuilder(store storage.Backend, signer *crypto.Signer) *APTBuilder {
	return &APTBuilder{
		Store:  store,
		Signer: signer,
		Origin: "svc.plus",
		Label:  "svc.plus Unified Distribution",
	}
}

// PublishSuite generates APT index metadata under apt/dists/<suite>/ and writes them to storage.Backend.
func (a *APTBuilder) PublishSuite(ctx context.Context, suite, component string, archs []string, packages []DebPackage) error {
	if suite == "" {
		suite = "stable"
	}
	if component == "" {
		component = "main"
	}
	if len(archs) == 0 {
		archs = []string{"amd64", "arm64"}
	}

	byArch := map[string][]DebPackage{}
	for _, arch := range archs {
		byArch[arch] = nil
	}
	for _, p := range packages {
		if p.Architecture == "all" {
			for _, arch := range archs {
				byArch[arch] = append(byArch[arch], p)
			}
		} else {
			byArch[p.Architecture] = append(byArch[p.Architecture], p)
		}
	}

	type relEntry struct {
		relPath string
		digests crypto.Digests
	}
	var relEntries []relEntry

	for _, arch := range archs {
		list := byArch[arch]
		sort.Slice(list, func(i, j int) bool {
			if list[i].Package != list[j].Package {
				return list[i].Package < list[j].Package
			}
			return list[i].Version < list[j].Version
		})
		var buf bytes.Buffer
		for _, p := range list {
			fmt.Fprintf(&buf, "Package: %s\n", p.Package)
			fmt.Fprintf(&buf, "Version: %s\n", p.Version)
			fmt.Fprintf(&buf, "Architecture: %s\n", p.Architecture)
			if p.Maintainer != "" {
				fmt.Fprintf(&buf, "Maintainer: %s\n", p.Maintainer)
			}
			if p.Section != "" {
				fmt.Fprintf(&buf, "Section: %s\n", p.Section)
			}
			if p.Priority != "" {
				fmt.Fprintf(&buf, "Priority: %s\n", p.Priority)
			}
			if p.Depends != "" {
				fmt.Fprintf(&buf, "Depends: %s\n", p.Depends)
			}
			if p.Homepage != "" {
				fmt.Fprintf(&buf, "Homepage: %s\n", p.Homepage)
			}
			fmt.Fprintf(&buf, "Filename: %s\n", strings.TrimPrefix(p.Filename, "/"))
			fmt.Fprintf(&buf, "Size: %d\n", p.Digests.Size)
			fmt.Fprintf(&buf, "MD5sum: %s\n", p.Digests.MD5)
			fmt.Fprintf(&buf, "SHA1: %s\n", p.Digests.SHA1)
			fmt.Fprintf(&buf, "SHA256: %s\n", p.Digests.SHA256)
			if p.Description != "" {
				fmt.Fprintf(&buf, "Description: %s\n", p.Description)
			} else {
				fmt.Fprintf(&buf, "Description: %s package\n", p.Package)
			}
			buf.WriteByte('\n')
		}

		rawPkg := buf.Bytes()
		var gzBuf bytes.Buffer
		gzw := gzip.NewWriter(&gzBuf)
		_, _ = gzw.Write(rawPkg)
		_ = gzw.Close()
		gzPkg := gzBuf.Bytes()

		relPlain := fmt.Sprintf("%s/binary-%s/Packages", component, arch)
		relGz := relPlain + ".gz"
		if err := a.Store.Put(ctx, fmt.Sprintf("apt/dists/%s/%s", suite, relPlain), bytes.NewReader(rawPkg), int64(len(rawPkg)), storage.PutOptions{
			ContentType:  "text/plain; charset=utf-8",
			CacheControl: "public, max-age=60, stale-while-revalidate=300",
		}); err != nil {
			return err
		}
		if err := a.Store.Put(ctx, fmt.Sprintf("apt/dists/%s/%s", suite, relGz), bytes.NewReader(gzPkg), int64(len(gzPkg)), storage.PutOptions{
			ContentType:  "application/gzip",
			CacheControl: "public, max-age=60, stale-while-revalidate=300",
		}); err != nil {
			return err
		}
		relEntries = append(relEntries,
			relEntry{relPath: relPlain, digests: crypto.ComputeDigestsBytes(rawPkg)},
			relEntry{relPath: relGz, digests: crypto.ComputeDigestsBytes(gzPkg)},
		)
	}

	sort.Slice(relEntries, func(i, j int) bool { return relEntries[i].relPath < relEntries[j].relPath })
	var relBuf bytes.Buffer
	fmt.Fprintf(&relBuf, "Origin: %s\n", a.Origin)
	fmt.Fprintf(&relBuf, "Label: %s\n", a.Label)
	fmt.Fprintf(&relBuf, "Suite: %s\n", suite)
	fmt.Fprintf(&relBuf, "Codename: %s\n", suite)
	fmt.Fprintf(&relBuf, "Date: %s\n", time.Now().UTC().Format(time.RFC1123))
	fmt.Fprintf(&relBuf, "Architectures: %s\n", strings.Join(archs, " "))
	fmt.Fprintf(&relBuf, "Components: %s\n", component)
	relBuf.WriteString("MD5Sum:\n")
	for _, e := range relEntries {
		fmt.Fprintf(&relBuf, " %s %16d %s\n", e.digests.MD5, e.digests.Size, e.relPath)
	}
	relBuf.WriteString("SHA256:\n")
	for _, e := range relEntries {
		fmt.Fprintf(&relBuf, " %s %16d %s\n", e.digests.SHA256, e.digests.Size, e.relPath)
	}
	relBytes := relBuf.Bytes()

	if err := a.Store.Put(ctx, fmt.Sprintf("apt/dists/%s/Release", suite), bytes.NewReader(relBytes), int64(len(relBytes)), storage.PutOptions{
		ContentType:  "text/plain; charset=utf-8",
		CacheControl: "public, max-age=60, stale-while-revalidate=300",
	}); err != nil {
		return err
	}

	if a.Signer != nil {
		inRelease, err := a.Signer.ClearSign(relBytes)
		if err != nil {
			return err
		}
		if err := a.Store.Put(ctx, fmt.Sprintf("apt/dists/%s/InRelease", suite), bytes.NewReader(inRelease), int64(len(inRelease)), storage.PutOptions{
			ContentType:  "text/plain; charset=utf-8",
			CacheControl: "public, max-age=60, stale-while-revalidate=300",
		}); err != nil {
			return err
		}
		relGPG, err := a.Signer.DetachedSignArmored(relBytes)
		if err != nil {
			return err
		}
		if err := a.Store.Put(ctx, fmt.Sprintf("apt/dists/%s/Release.gpg", suite), bytes.NewReader(relGPG), int64(len(relGPG)), storage.PutOptions{
			ContentType:  "application/pgp-signature",
			CacheControl: "public, max-age=60, stale-while-revalidate=300",
		}); err != nil {
			return err
		}
		pubBin, err := a.Signer.PublicKeyBinary()
		if err != nil {
			return err
		}
		if err := a.Store.Put(ctx, "apt/svc-plus-archive-keyring.gpg", bytes.NewReader(pubBin), int64(len(pubBin)), storage.PutOptions{
			ContentType:  "application/pgp-keys",
			CacheControl: "public, max-age=3600",
		}); err != nil {
			return err
		}
	}
	return nil
}
