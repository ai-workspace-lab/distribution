#!/usr/bin/env bash
set -euo pipefail

# Build an APT repository under <output-dir>/apt/ with pool/main/<product>/,
# dists/{stable,testing}/main/binary-{amd64,arm64}/Packages{,.gz}, Release,
# and optional GPG signing (InRelease, Release.gpg, svc-plus-archive-keyring.gpg).

SOURCE_DEBS=""
OUTPUT_DIR=""
GPG_KEY_ID="${APT_GPG_KEY_ID:-${GPG_KEY_ID:-}}"
GPG_PASSPHRASE="${APT_GPG_PASSPHRASE:-${GPG_PASSPHRASE:-}}"
CHANNELS="stable,testing"
ORIGIN="svc.plus"
LABEL="svc.plus APT Repository"
DRY_RUN=false

usage() {
  cat <<'EOF'
Usage: build_apt_repo.sh [options]

Options:
  --source-debs <dir>   Directory containing .deb packages (optional if building empty skeleton)
  --output-dir <dir>    Root output directory; APT repo is written to <output-dir>/apt (required)
  --gpg-key-id <id>     GPG key fingerprint/ID for signing Release/InRelease (optional in --dry-run)
  --channels <csv>      Comma-separated distributions (default: stable,testing)
  --origin <name>       APT Origin header (default: svc.plus)
  --label <label>       APT Label header (default: svc.plus APT Repository)
  --dry-run             Generate metadata preview without requiring GPG signing
  -h, --help            Show this help message
EOF
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --source-debs)
      SOURCE_DEBS="${2:-}"
      shift 2
      ;;
    --output-dir)
      OUTPUT_DIR="${2:-}"
      shift 2
      ;;
    --gpg-key-id)
      GPG_KEY_ID="${2:-}"
      shift 2
      ;;
    --channels)
      CHANNELS="${2:-}"
      shift 2
      ;;
    --origin)
      ORIGIN="${2:-}"
      shift 2
      ;;
    --label)
      LABEL="${2:-}"
      shift 2
      ;;
    --dry-run)
      DRY_RUN=true
      shift
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      echo "Unknown option: $1" >&2
      usage >&2
      exit 1
      ;;
  esac
done

if [[ -z "$OUTPUT_DIR" ]]; then
  echo "Error: --output-dir is required." >&2
  usage >&2
  exit 1
fi

APT_ROOT="${OUTPUT_DIR%/}/apt"
mkdir -p "${APT_ROOT}/pool/main"

infer_product_from_deb() {
  local deb_path="$1"
  local base
  base="$(basename "$deb_path")"
  case "$base" in
    xconnect*|XConnect*) echo "xconnect" ;;
    xworkmate*|XWorkmate*) echo "xworkmate" ;;
    *)
      local parent
      parent="$(basename "$(dirname "$deb_path")")"
      if [[ "$parent" != "." && "$parent" != "/" && -n "$parent" ]]; then
        echo "$parent" | tr '[:upper:]' '[:lower:]'
      else
        echo "${base%%[-_]*}" | tr '[:upper:]' '[:lower:]'
      fi
      ;;
  esac
}

infer_arch_from_deb() {
  local deb_path="$1"
  if command -v dpkg-deb >/dev/null 2>&1; then
    local detected
    detected="$(dpkg-deb -f "$deb_path" Architecture 2>/dev/null || true)"
    if [[ "$detected" == "amd64" || "$detected" == "arm64" || "$detected" == "all" ]]; then
      echo "$detected"
      return 0
    fi
  fi
  local base
  base="$(basename "$deb_path")"
  case "$base" in
    *arm64*|*aarch64*) echo "arm64" ;;
    *) echo "amd64" ;;
  esac
}

if [[ -n "$SOURCE_DEBS" ]]; then
  if [[ ! -d "$SOURCE_DEBS" ]]; then
    echo "Error: --source-debs directory not found: $SOURCE_DEBS" >&2
    exit 1
  fi
  while IFS= read -r deb_file; do
    [[ -n "$deb_file" ]] || continue
    product="$(infer_product_from_deb "$deb_file")"
    mkdir -p "${APT_ROOT}/pool/main/${product}"
    cp -f "$deb_file" "${APT_ROOT}/pool/main/${product}/$(basename "$deb_file")"
  done < <(find "$SOURCE_DEBS" -type f -name '*.deb' | sort)
fi

generate_packages_python() {
  local apt_root="$1"
  local dist="$2"
  local arch="$3"
  local packages_file="$4"
  python3 - "$apt_root" "$dist" "$arch" "$packages_file" <<'PYEOF'
import gzip
import hashlib
import os
from pathlib import Path
import re
import subprocess
import sys

apt_root = Path(sys.argv[1])
arch = sys.argv[3]
packages_file = Path(sys.argv[4])

pool_dir = apt_root / "pool" / "main"
deb_files = sorted(pool_dir.rglob("*.deb")) if pool_dir.exists() else []


def extract_deb_control(deb_path: Path) -> dict[str, str]:
    # Prefer dpkg-deb when available and the file is a valid Debian archive.
    if subprocess.run(["which", "dpkg-deb"], capture_output=True).returncode == 0:
        proc = subprocess.run(
            ["dpkg-deb", "-f", str(deb_path)],
            capture_output=True,
            text=True,
        )
        if proc.returncode == 0 and proc.stdout.strip():
            meta: dict[str, str] = {}
            last_key = ""
            for line in proc.stdout.splitlines():
                if line.startswith((" ", "\t")) and last_key:
                    meta[last_key] += "\n" + line
                elif ":" in line:
                    k, v = line.split(":", 1)
                    last_key = k.strip()
                    meta[last_key] = v.strip()
            return meta

    name = deb_path.name
    stem = deb_path.stem
    pkg_arch = "arm64" if ("arm64" in stem.lower() or "aarch64" in stem.lower()) else "amd64"
    m = re.match(r"^([a-zA-Z0-9.+_-]+?)[_-](\d+\.\d+\.\d+[^_-]*)[_-](amd64|arm64|all)$", stem)
    if m:
        pkg_name, version, pkg_arch = m.group(1), m.group(2), m.group(3)
    else:
        pkg_name = deb_path.parent.name if deb_path.parent.name != "main" else stem.split("-")[0].split("_")[0]
        version_match = re.search(r"(\d+\.\d+\.\d+(?:[-.+~][a-zA-Z0-9]+)*)", stem)
        version = version_match.group(1) if version_match else "0.0.0"

    return {
        "Package": pkg_name.lower(),
        "Version": version,
        "Architecture": pkg_arch,
        "Maintainer": "svc.plus Release Engineering <release@svc.plus>",
        "Section": "net",
        "Priority": "optional",
        "Description": f"{pkg_name} desktop package",
    }


stanzas: list[str] = []
for deb in deb_files:
    meta = extract_deb_control(deb)
    deb_arch = meta.get("Architecture", "amd64")
    if deb_arch not in (arch, "all"):
        continue
    data = deb.read_bytes()
    size = len(data)
    md5 = hashlib.md5(data).hexdigest()
    sha1 = hashlib.sha1(data).hexdigest()
    sha256 = hashlib.sha256(data).hexdigest()
    rel_path = deb.relative_to(apt_root).as_posix()

    lines = [
        f"Package: {meta.get('Package', deb.parent.name)}",
        f"Version: {meta.get('Version', '0.0.0')}",
        f"Architecture: {deb_arch}",
        f"Maintainer: {meta.get('Maintainer', 'svc.plus <release@svc.plus>')}",
        f"Installed-Size: {meta.get('Installed-Size', str(max(1, size // 1024)))}",
        f"Filename: {rel_path}",
        f"Size: {size}",
        f"MD5sum: {md5}",
        f"SHA1: {sha1}",
        f"SHA256: {sha256}",
        f"Section: {meta.get('Section', 'net')}",
        f"Priority: {meta.get('Priority', 'optional')}",
        f"Description: {meta.get('Description', deb.stem)}",
    ]
    stanzas.append("\n".join(lines))

content = ("\n\n".join(stanzas) + "\n") if stanzas else ""
packages_file.write_text(content, encoding="utf-8")
with gzip.open(str(packages_file) + ".gz", "wb") as gz:
    gz.write(content.encode("utf-8"))
PYEOF
}

generate_release_file() {
  local dist_dir="$1"
  local dist="$2"
  local origin="$3"
  local label="$4"
  python3 - "$dist_dir" "$dist" "$origin" "$label" <<'PYEOF'
from datetime import datetime, timezone
import hashlib
from pathlib import Path
import sys

dist_dir = Path(sys.argv[1])
dist = sys.argv[2]
origin = sys.argv[3]
label = sys.argv[4]

date_str = datetime.now(timezone.utc).strftime("%a, %d %b %Y %H:%M:%S UTC")
tracked = sorted(
    p for p in dist_dir.rglob("*")
    if p.is_file() and p.name in {"Packages", "Packages.gz"}
)

md5_lines = []
sha256_lines = []
for f in tracked:
    rel = f.relative_to(dist_dir).as_posix()
    raw = f.read_bytes()
    md5_lines.append(f" {hashlib.md5(raw).hexdigest()} {len(raw):16d} {rel}")
    sha256_lines.append(f" {hashlib.sha256(raw).hexdigest()} {len(raw):16d} {rel}")

lines = [
    f"Origin: {origin}",
    f"Label: {label}",
    f"Suite: {dist}",
    f"Codename: {dist}",
    f"Date: {date_str}",
    "Architectures: amd64 arm64",
    "Components: main",
    f"Description: {label} ({dist})",
    "MD5Sum:",
    *md5_lines,
    "SHA256:",
    *sha256_lines,
]
(dist_dir / "Release").write_text("\n".join(lines) + "\n", encoding="utf-8")
PYEOF
}

IFS=',' read -r -a CHANNEL_LIST <<< "$CHANNELS"
for raw_dist in "${CHANNEL_LIST[@]}"; do
  dist="$(echo "$raw_dist" | xargs)"
  [[ -n "$dist" ]] || continue
  dist_dir="${APT_ROOT}/dists/${dist}"
  for arch in amd64 arm64; do
    bin_dir="${dist_dir}/main/binary-${arch}"
    mkdir -p "$bin_dir"
    generate_packages_python "$APT_ROOT" "$dist" "$arch" "${bin_dir}/Packages"
  done
  generate_release_file "$dist_dir" "$dist" "$ORIGIN" "$LABEL"

  if [[ "$DRY_RUN" == "true" ]]; then
    echo "[dry-run] Unsigned APT preview generated for dist=${dist}: ${dist_dir}/Release"
  elif [[ -n "$GPG_KEY_ID" ]] && command -v gpg >/dev/null 2>&1; then
    gpg_opts=(--batch --yes --pinentry-mode loopback)
    if [[ -n "$GPG_PASSPHRASE" ]]; then
      gpg_opts+=(--passphrase "$GPG_PASSPHRASE")
    fi
    gpg "${gpg_opts[@]}" --local-user "$GPG_KEY_ID" -abs -o "${dist_dir}/Release.gpg" "${dist_dir}/Release"
    gpg "${gpg_opts[@]}" --local-user "$GPG_KEY_ID" --clearsign -o "${dist_dir}/InRelease" "${dist_dir}/Release"
    echo "Signed APT Release/InRelease for dist=${dist} with key ${GPG_KEY_ID}"
  else
    echo "Notice: GPG_KEY_ID not set (or gpg unavailable); generated unsigned APT preview for dist=${dist}."
  fi
done

if [[ "$DRY_RUN" != "true" && -n "$GPG_KEY_ID" ]] && command -v gpg >/dev/null 2>&1; then
  gpg --batch --yes --export "$GPG_KEY_ID" > "${APT_ROOT}/svc-plus-archive-keyring.gpg"
  gpg --batch --yes --armor --export "$GPG_KEY_ID" > "${APT_ROOT}/svc-plus.asc"
fi

echo "APT repository built at ${APT_ROOT}"
