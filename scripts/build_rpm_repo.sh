#!/usr/bin/env bash
set -euo pipefail

# Build a YUM/DNF RPM repository under <output-dir>/rpm/{stable,testing}/{x86_64,aarch64}/
# using rpmsign, createrepo_c, and GPG-signed repomd.xml.asc (with clean --dry-run preview).

SOURCE_RPMS=""
OUTPUT_DIR=""
GPG_KEY_ID="${RPM_GPG_KEY_ID:-${GPG_KEY_ID:-}}"
CHANNELS="stable,testing"
MIRROR_URL="https://repo.svc.plus"
DRY_RUN=false

usage() {
  cat <<'EOF'
Usage: build_rpm_repo.sh [options]

Options:
  --source-rpms <dir>   Directory containing .rpm packages (optional if building empty skeleton)
  --output-dir <dir>    Root output directory; RPM repo is written to <output-dir>/rpm (required)
  --gpg-key-id <id>     GPG key fingerprint/ID for rpmsign and repomd.xml.asc (optional in --dry-run)
  --channels <csv>      Comma-separated channels (default: stable,testing)
  --mirror-url <url>    Repository base URL written into svc-plus.repo (default: https://repo.svc.plus)
  --dry-run             Generate metadata preview without requiring rpmsign/createrepo_c/GPG
  -h, --help            Show this help message
EOF
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --source-rpms)
      SOURCE_RPMS="${2:-}"
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
    --mirror-url)
      MIRROR_URL="${2:-}"
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

RPM_ROOT="${OUTPUT_DIR%/}/rpm"
MIRROR_URL="${MIRROR_URL%/}"

infer_rpm_arch() {
  local rpm_path="$1"
  local base
  base="$(basename "$rpm_path")"
  case "$base" in
    *aarch64*|*arm64*) echo "aarch64" ;;
    *) echo "x86_64" ;;
  esac
}

IFS=',' read -r -a CHANNEL_LIST <<< "$CHANNELS"
for raw_ch in "${CHANNEL_LIST[@]}"; do
  ch="$(echo "$raw_ch" | xargs)"
  [[ -n "$ch" ]] || continue
  for arch in x86_64 aarch64; do
    mkdir -p "${RPM_ROOT}/${ch}/${arch}"
  done
done

if [[ -n "$SOURCE_RPMS" ]]; then
  if [[ ! -d "$SOURCE_RPMS" ]]; then
    echo "Error: --source-rpms directory not found: $SOURCE_RPMS" >&2
    exit 1
  fi
  first_channel="$(echo "${CHANNEL_LIST[0]}" | xargs)"
  while IFS= read -r rpm_file; do
    [[ -n "$rpm_file" ]] || continue
    arch="$(infer_rpm_arch "$rpm_file")"
    dest="${RPM_ROOT}/${first_channel}/${arch}/$(basename "$rpm_file")"
    cp -f "$rpm_file" "$dest"
    if [[ "$DRY_RUN" == "true" ]]; then
      if [[ -n "$GPG_KEY_ID" ]]; then
        echo "[dry-run] rpmsign --addsign --key-id=${GPG_KEY_ID} ${dest}"
      fi
    elif [[ -n "$GPG_KEY_ID" ]] && command -v rpmsign >/dev/null 2>&1; then
      rpmsign --define "_gpg_name ${GPG_KEY_ID}" --addsign "$dest"
    fi
  done < <(find "$SOURCE_RPMS" -type f -name '*.rpm' | sort)
fi

write_repomd_preview() {
  local repo_dir="$1"
  python3 - "$repo_dir" <<'PYEOF'
from datetime import datetime, timezone
import gzip
import hashlib
from pathlib import Path
import sys

repo_dir = Path(sys.argv[1])
repodata_dir = repo_dir / "repodata"
repodata_dir.mkdir(parents=True, exist_ok=True)
rpms = sorted(repo_dir.glob("*.rpm"))

pkg_entries = []
for rpm in rpms:
    sha = hashlib.sha256(rpm.read_bytes()).hexdigest()
    size = rpm.stat().st_size
    pkg_entries.append(
        f'  <package type="rpm"><name>{rpm.stem}</name>'
        f'<checksum type="sha256" pkgid="YES">{sha}</checksum>'
        f'<size package="{size}" installed="{size}" archive="{size}"/>'
        f'<location href="{rpm.name}"/></package>'
    )

primary_xml = (
    '<?xml version="1.0" encoding="UTF-8"?>\n'
    f'<metadata xmlns="http://linux.duke.edu/metadata/common" packages="{len(rpms)}">\n'
    + "\n".join(pkg_entries)
    + ("\n" if pkg_entries else "")
    + "</metadata>\n"
)
primary_gz = gzip.compress(primary_xml.encode("utf-8"))
primary_sha = hashlib.sha256(primary_gz).hexdigest()
primary_open_sha = hashlib.sha256(primary_xml.encode("utf-8")).hexdigest()
(repodata_dir / "primary.xml.gz").write_bytes(primary_gz)

ts = int(datetime.now(timezone.utc).timestamp())
repomd_xml = f"""<?xml version="1.0" encoding="UTF-8"?>
<repomd xmlns="http://linux.duke.edu/metadata/repo">
  <revision>{ts}</revision>
  <data type="primary">
    <checksum type="sha256">{primary_sha}</checksum>
    <open-checksum type="sha256">{primary_open_sha}</open-checksum>
    <location href="repodata/primary.xml.gz"/>
    <timestamp>{ts}</timestamp>
    <size>{len(primary_gz)}</size>
    <open-size>{len(primary_xml.encode("utf-8"))}</open-size>
  </data>
</repomd>
"""
(repodata_dir / "repomd.xml").write_text(repomd_xml, encoding="utf-8")
PYEOF
}

for raw_ch in "${CHANNEL_LIST[@]}"; do
  ch="$(echo "$raw_ch" | xargs)"
  [[ -n "$ch" ]] || continue
  for arch in x86_64 aarch64; do
    target="${RPM_ROOT}/${ch}/${arch}"
    if [[ "$DRY_RUN" != "true" ]] && command -v createrepo_c >/dev/null 2>&1; then
      createrepo_c --update "$target"
    else
      if [[ "$DRY_RUN" == "true" ]]; then
        echo "[dry-run] createrepo_c --update ${target} (writing repodata preview)"
      else
        echo "Notice: createrepo_c unavailable; writing repodata preview for ${target}"
      fi
      write_repomd_preview "$target"
    fi

    if [[ "$DRY_RUN" == "true" ]]; then
      if [[ -n "$GPG_KEY_ID" ]]; then
        echo "[dry-run] gpg --batch --yes --detach-sign --armor -u ${GPG_KEY_ID} -o ${target}/repodata/repomd.xml.asc ${target}/repodata/repomd.xml"
      fi
    elif [[ -n "$GPG_KEY_ID" ]] && command -v gpg >/dev/null 2>&1; then
      gpg --batch --yes --local-user "$GPG_KEY_ID" --detach-sign --armor \
        -o "${target}/repodata/repomd.xml.asc" "${target}/repodata/repomd.xml"
    fi
  done
done

if [[ "$DRY_RUN" != "true" && -n "$GPG_KEY_ID" ]] && command -v gpg >/dev/null 2>&1; then
  gpg --batch --yes --armor --export "$GPG_KEY_ID" > "${RPM_ROOT}/RPM-GPG-KEY-svc-plus"
fi

cat > "${RPM_ROOT}/svc-plus.repo" <<EOF
[svc-plus]
name=svc.plus Repository - Stable
baseurl=${MIRROR_URL}/rpm/stable/\$basearch
enabled=1
gpgcheck=1
repo_gpgcheck=1
gpgkey=${MIRROR_URL}/rpm/RPM-GPG-KEY-svc-plus

[svc-plus-testing]
name=svc.plus Repository - Testing
baseurl=${MIRROR_URL}/rpm/testing/\$basearch
enabled=0
gpgcheck=1
repo_gpgcheck=1
gpgkey=${MIRROR_URL}/rpm/RPM-GPG-KEY-svc-plus
EOF

echo "RPM repository built at ${RPM_ROOT}"
