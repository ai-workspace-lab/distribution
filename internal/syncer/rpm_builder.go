//go:build !js

package syncer

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/binary"
	"encoding/xml"
	"fmt"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/ai-workspace-lab/distribution/internal/crypto"
	"github.com/ai-workspace-lab/distribution/internal/storage"
)

// RPMPackage holds parsed RPM header metadata and storage location for YUM/DNF repodata generation.
type RPMPackage struct {
	Name        string
	Version     string
	Release     string
	Epoch       string
	Arch        string
	Summary     string
	Description string
	License     string
	URL         string
	Location    string
	Digests     crypto.Digests
	BuildTime   int64
}

// ParseRPMHeader parses an RPM v3/v4 binary header in pure Go (no `rpm` or `createrepo_c` binary required).
func ParseRPMHeader(rpmBytes []byte, fallbackLocation string) (RPMPackage, error) {
	pkg := RPMPackage{
		Location:  fallbackLocation,
		Epoch:     "0",
		Arch:      "x86_64",
		Release:   "1",
		Digests:   crypto.ComputeDigestsBytes(rpmBytes),
		BuildTime: time.Now().UTC().Unix(),
	}
	if len(rpmBytes) < 96+16 || rpmBytes[0] != 0xed || rpmBytes[1] != 0xab || rpmBytes[2] != 0xee || rpmBytes[3] != 0xdb {
		return pkg, fmt.Errorf("not a valid RPM binary file")
	}
	sigEnd, err := skipRPMHeader(rpmBytes, 96, true)
	if err != nil {
		return pkg, fmt.Errorf("skip rpm signature header: %w", err)
	}
	tags, err := readRPMHeaderTags(rpmBytes, sigEnd)
	if err != nil {
		return pkg, fmt.Errorf("read rpm main header: %w", err)
	}
	if v := tags[1000]; v != "" {
		pkg.Name = v
	}
	if v := tags[1001]; v != "" {
		pkg.Version = v
	}
	if v := tags[1002]; v != "" {
		pkg.Release = v
	}
	if v := tags[1004]; v != "" {
		pkg.Summary = v
	}
	if v := tags[1005]; v != "" {
		pkg.Description = v
	}
	if v := tags[1014]; v != "" {
		pkg.License = v
	}
	if v := tags[1020]; v != "" {
		pkg.URL = v
	}
	if v := tags[1022]; v != "" {
		pkg.Arch = v
	}
	if pkg.Name == "" {
		pkg.Name = strings.TrimSuffix(path.Base(fallbackLocation), ".rpm")
	}
	return pkg, nil
}

func skipRPMHeader(data []byte, offset int, align8 bool) (int, error) {
	if offset+16 > len(data) {
		return 0, fmt.Errorf("truncated rpm header")
	}
	if data[offset] != 0x8e || data[offset+1] != 0xad || data[offset+2] != 0xe8 {
		return 0, fmt.Errorf("missing rpm header magic at %d", offset)
	}
	nindex := int(binary.BigEndian.Uint32(data[offset+8 : offset+12]))
	hsize := int(binary.BigEndian.Uint32(data[offset+12 : offset+16]))
	end := offset + 16 + nindex*16 + hsize
	if align8 {
		if rem := end % 8; rem != 0 {
			end += 8 - rem
		}
	}
	if end > len(data) {
		return 0, fmt.Errorf("rpm header extends past EOF")
	}
	return end, nil
}

func readRPMHeaderTags(data []byte, offset int) (map[uint32]string, error) {
	if offset+16 > len(data) {
		return nil, fmt.Errorf("truncated rpm main header")
	}
	if data[offset] != 0x8e || data[offset+1] != 0xad || data[offset+2] != 0xe8 {
		return nil, fmt.Errorf("missing rpm main header magic")
	}
	nindex := int(binary.BigEndian.Uint32(data[offset+8 : offset+12]))
	hsize := int(binary.BigEndian.Uint32(data[offset+12 : offset+16]))
	indexStart := offset + 16
	storeStart := indexStart + nindex*16
	if storeStart+hsize > len(data) {
		return nil, fmt.Errorf("rpm store extends past EOF")
	}
	store := data[storeStart : storeStart+hsize]
	out := map[uint32]string{}
	for i := 0; i < nindex; i++ {
		e := data[indexStart+i*16 : indexStart+(i+1)*16]
		tag := binary.BigEndian.Uint32(e[0:4])
		typ := binary.BigEndian.Uint32(e[4:8])
		off := int(binary.BigEndian.Uint32(e[8:12]))
		if (typ == 6 || typ == 9) && off >= 0 && off < len(store) {
			end := bytes.IndexByte(store[off:], 0)
			if end >= 0 {
				out[tag] = string(store[off : off+end])
			}
		}
	}
	return out, nil
}

// RPMBuilder creates YUM/DNF repodata (repomd.xml, primary.xml.gz, filelists.xml.gz, other.xml.gz, repomd.xml.asc) in pure Go.
type RPMBuilder struct {
	Store  storage.Backend
	Signer *crypto.Signer
}

func NewRPMBuilder(store storage.Backend, signer *crypto.Signer) *RPMBuilder {
	return &RPMBuilder{Store: store, Signer: signer}
}

// PublishRepo writes repodata under rpm/<channel>/<arch>/repodata/ into storage.Backend.
func (r *RPMBuilder) PublishRepo(ctx context.Context, channel, arch string, packages []RPMPackage) error {
	if channel == "" {
		channel = "stable"
	}
	if arch == "" {
		arch = "x86_64"
	}
	sort.Slice(packages, func(i, j int) bool {
		if packages[i].Name != packages[j].Name {
			return packages[i].Name < packages[j].Name
		}
		return packages[i].Version < packages[j].Version
	})

	primaryXML := renderPrimaryXML(packages)
	filelistsXML := renderEmptyXML("filelists", "http://linux.duke.edu/metadata/filelists", len(packages))
	otherXML := renderEmptyXML("other", "http://linux.duke.edu/metadata/other", len(packages))

	primaryGz := gzipBytes(primaryXML)
	filelistsGz := gzipBytes(filelistsXML)
	otherGz := gzipBytes(otherXML)

	basePrefix := fmt.Sprintf("rpm/%s/%s", storage.CleanKey(channel), storage.CleanKey(arch))
	now := time.Now().UTC().Unix()

	type mdEntry struct {
		typ      string
		filename string
		openDig  crypto.Digests
		gzDig    crypto.Digests
		gzBytes  []byte
	}
	entries := []mdEntry{
		{typ: "primary", filename: "repodata/primary.xml.gz", openDig: crypto.ComputeDigestsBytes(primaryXML), gzDig: crypto.ComputeDigestsBytes(primaryGz), gzBytes: primaryGz},
		{typ: "filelists", filename: "repodata/filelists.xml.gz", openDig: crypto.ComputeDigestsBytes(filelistsXML), gzDig: crypto.ComputeDigestsBytes(filelistsGz), gzBytes: filelistsGz},
		{typ: "other", filename: "repodata/other.xml.gz", openDig: crypto.ComputeDigestsBytes(otherXML), gzDig: crypto.ComputeDigestsBytes(otherGz), gzBytes: otherGz},
	}

	var repomd bytes.Buffer
	repomd.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	repomd.WriteString(`<repomd xmlns="http://linux.duke.edu/metadata/repo" xmlns:rpm="http://linux.duke.edu/metadata/rpm">` + "\n")
	fmt.Fprintf(&repomd, "  <revision>%d</revision>\n", now)
	for _, e := range entries {
		if err := r.Store.Put(ctx, basePrefix+"/"+e.filename, bytes.NewReader(e.gzBytes), int64(len(e.gzBytes)), storage.PutOptions{
			ContentType:  "application/gzip",
			CacheControl: "public, max-age=60, stale-while-revalidate=300",
		}); err != nil {
			return err
		}
		fmt.Fprintf(&repomd, `  <data type="%s">
    <checksum type="sha256">%s</checksum>
    <open-checksum type="sha256">%s</open-checksum>
    <location href="%s"/>
    <timestamp>%d</timestamp>
    <size>%d</size>
    <open-size>%d</open-size>
  </data>
`, e.typ, e.gzDig.SHA256, e.openDig.SHA256, e.filename, now, e.gzDig.Size, e.openDig.Size)
	}
	repomd.WriteString("</repomd>\n")
	repomdBytes := repomd.Bytes()

	if err := r.Store.Put(ctx, basePrefix+"/repodata/repomd.xml", bytes.NewReader(repomdBytes), int64(len(repomdBytes)), storage.PutOptions{
		ContentType:  "application/xml",
		CacheControl: "public, max-age=60, stale-while-revalidate=300",
	}); err != nil {
		return err
	}

	if r.Signer != nil {
		asc, err := r.Signer.DetachedSignArmored(repomdBytes)
		if err != nil {
			return err
		}
		if err := r.Store.Put(ctx, basePrefix+"/repodata/repomd.xml.asc", bytes.NewReader(asc), int64(len(asc)), storage.PutOptions{
			ContentType:  "application/pgp-signature",
			CacheControl: "public, max-age=60, stale-while-revalidate=300",
		}); err != nil {
			return err
		}
		pubArmored, err := r.Signer.PublicKeyArmored()
		if err != nil {
			return err
		}
		if err := r.Store.Put(ctx, "rpm/RPM-GPG-KEY-svc-plus", bytes.NewReader(pubArmored), int64(len(pubArmored)), storage.PutOptions{
			ContentType:  "application/pgp-keys",
			CacheControl: "public, max-age=3600",
		}); err != nil {
			return err
		}
	}
	return nil
}

func renderPrimaryXML(packages []RPMPackage) []byte {
	var b bytes.Buffer
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	fmt.Fprintf(&b, `<metadata xmlns="http://linux.duke.edu/metadata/common" xmlns:rpm="http://linux.duke.edu/metadata/rpm" packages="%d">`+"\n", len(packages))
	for _, p := range packages {
		fmt.Fprintf(&b, `  <package type="rpm">
    <name>%s</name>
    <arch>%s</arch>
    <version epoch="%s" ver="%s" rel="%s"/>
    <checksum type="sha256" pkgid="YES">%s</checksum>
    <summary>%s</summary>
    <description>%s</description>
    <url>%s</url>
    <time file="%d" build="%d"/>
    <size package="%d" installed="%d" archive="%d"/>
    <location href="%s"/>
    <format>
      <rpm:license>%s</rpm:license>
    </format>
  </package>
`, xmlEscape(p.Name), xmlEscape(p.Arch), xmlEscape(p.Epoch), xmlEscape(p.Version), xmlEscape(p.Release),
			p.Digests.SHA256, xmlEscape(p.Summary), xmlEscape(p.Description), xmlEscape(p.URL),
			p.BuildTime, p.BuildTime, p.Digests.Size, p.Digests.Size, p.Digests.Size,
			xmlEscape(strings.TrimPrefix(p.Location, "/")), xmlEscape(p.License))
	}
	b.WriteString("</metadata>\n")
	return b.Bytes()
}

func renderEmptyXML(root, ns string, count int) []byte {
	return []byte(fmt.Sprintf("<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n<%s xmlns=\"%s\" packages=\"%d\"/>\n", root, ns, count))
}

func xmlEscape(s string) string {
	var b bytes.Buffer
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

func gzipBytes(in []byte) []byte {
	var b bytes.Buffer
	gw := gzip.NewWriter(&b)
	_, _ = gw.Write(in)
	_ = gw.Close()
	return b.Bytes()
}
