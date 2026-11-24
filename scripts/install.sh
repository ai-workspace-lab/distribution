#!/bin/sh
# Universal POSIX sh installer for svc.plus / ai-workspace-lab applications.
# Supports macOS (Homebrew Cask / .dmg) and Linux (APT, DNF/YUM, .deb, .rpm, AppImage, tar.gz).
set -eu

PRODUCT="xconnect"
MIRROR="${SVC_PLUS_MIRROR:-auto}"
CHANNEL="${SVC_PLUS_CHANNEL:-latest}"
METHOD="auto"
SETUP_REPO=0
DRY_RUN=0
VERIFY_SHA256=1
OS_OVERRIDE="${SVC_PLUS_OS:-}"
ARCH_OVERRIDE="${SVC_PLUS_ARCH:-}"
PKG_MGR_OVERRIDE="${SVC_PLUS_PKG_MGR:-}"

usage() {
  cat <<'EOF'
svc.plus Universal Installer (POSIX sh)

Usage:
  install.sh [options]

Options:
  --product <name>     Product to install: xconnect | xconnect-app | xconnect-one | xconnect-gateway | xconnect-edge-agent | fluxgate | xworkmate | xworkmate-remote-desktop (default: xconnect)
  --mirror <mode>      Mirror selection: auto | global | cn (default: auto)
  --channel <name>     Release channel: latest | stable (default: latest)
  --method <mode>      Install method: auto | brew | dmg | apt | dnf | yum | deb | rpm | appimage | tar.gz | binary
  --setup-repo         Configure OS package repository / tap (APT, YUM/DNF, or Homebrew)
  --dry-run            Print planned actions and commands without modifying the system
  --no-verify          Skip SHA-256 checksum verification on direct downloads
  --os <os>            Override detected OS (macos | linux) for testing/dry-run
  --arch <arch>        Override detected arch (amd64 | arm64) for testing/dry-run
  --pkg-mgr <mgr>      Override detected package manager (brew | apt | dnf | yum | none)
  -h, --help           Show this help message

Examples:
  curl -fsSL https://repo.svc.plus/install.sh | sh -s -- --product xconnect
  curl -fsSL https://cn-repo.svc.plus/install.sh | sh -s -- --mirror cn --product xworkmate
  sh install.sh --setup-repo --mirror global --dry-run
EOF
}

log_info() {
  printf '[svc-plus] %s\n' "$*"
}

log_warn() {
  printf '[svc-plus][warn] %s\n' "$*" >&2
}

die() {
  printf '[svc-plus][error] %s\n' "$*" >&2
  exit 1
}

run_cmd() {
  if [ "$DRY_RUN" -eq 1 ]; then
    printf '[dry-run] %s\n' "$*"
    return 0
  fi
  "$@"
}

run_root() {
  if [ "$DRY_RUN" -eq 1 ]; then
    if [ "$(id -u 2>/dev/null || echo 1)" -ne 0 ]; then
      printf '[dry-run] sudo %s\n' "$*"
    else
      printf '[dry-run] %s\n' "$*"
    fi
    return 0
  fi
  if [ "$(id -u)" -ne 0 ]; then
    if command -v sudo >/dev/null 2>&1; then
      sudo "$@"
    else
      die "Root privileges required for: $*"
    fi
  else
    "$@"
  fi
}

while [ $# -gt 0 ]; do
  case "$1" in
    --product)
      [ $# -ge 2 ] || die "--product requires a value"
      PRODUCT="$2"
      shift 2
      ;;
    --product=*)
      PRODUCT="${1#*=}"
      shift
      ;;
    --mirror)
      [ $# -ge 2 ] || die "--mirror requires a value"
      MIRROR="$2"
      shift 2
      ;;
    --mirror=*)
      MIRROR="${1#*=}"
      shift
      ;;
    --channel)
      [ $# -ge 2 ] || die "--channel requires a value"
      CHANNEL="$2"
      shift 2
      ;;
    --channel=*)
      CHANNEL="${1#*=}"
      shift
      ;;
    --method)
      [ $# -ge 2 ] || die "--method requires a value"
      METHOD="$2"
      shift 2
      ;;
    --method=*)
      METHOD="${1#*=}"
      shift
      ;;
    --setup-repo)
      SETUP_REPO=1
      shift
      ;;
    --dry-run)
      DRY_RUN=1
      shift
      ;;
    --no-verify)
      VERIFY_SHA256=0
      shift
      ;;
    --os)
      [ $# -ge 2 ] || die "--os requires a value"
      OS_OVERRIDE="$2"
      shift 2
      ;;
    --os=*)
      OS_OVERRIDE="${1#*=}"
      shift
      ;;
    --arch)
      [ $# -ge 2 ] || die "--arch requires a value"
      ARCH_OVERRIDE="$2"
      shift 2
      ;;
    --arch=*)
      ARCH_OVERRIDE="${1#*=}"
      shift
      ;;
    --pkg-mgr)
      [ $# -ge 2 ] || die "--pkg-mgr requires a value"
      PKG_MGR_OVERRIDE="$2"
      shift 2
      ;;
    --pkg-mgr=*)
      PKG_MGR_OVERRIDE="${1#*=}"
      shift
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      die "Unknown option: $1 (use --help for usage)"
      ;;
  esac
done

case "$PRODUCT" in
  xconnect-app) PRODUCT="xconnect" ;;
esac

case "$PRODUCT" in
  xconnect|xworkmate|xconnect-one|xconnect-gateway|xconnect-edge-agent|fluxgate|xworkmate-remote-desktop) ;;
  *)
    die "Unsupported product '$PRODUCT'. Expected one of: xconnect, xconnect-app, xconnect-one, xconnect-gateway, xconnect-edge-agent, fluxgate, xworkmate, xworkmate-remote-desktop"
    ;;
esac

case "$MIRROR" in
  auto|global|cn) ;;
  *)
    die "Unsupported mirror '$MIRROR'. Expected one of: auto, global, cn"
    ;;
esac

case "$CHANNEL" in
  latest|stable) ;;
  *)
    die "Unsupported channel '$CHANNEL'. Expected one of: latest, stable"
    ;;
esac

detect_os() {
  if [ -n "$OS_OVERRIDE" ]; then
    case "$OS_OVERRIDE" in
      macos|darwin|Darwin) printf 'macos' ;;
      linux|Linux) printf 'linux' ;;
      *) die "Unsupported --os override: $OS_OVERRIDE" ;;
    esac
    return 0
  fi
  uname_s="$(uname -s 2>/dev/null || echo Linux)"
  case "$uname_s" in
    Darwin) printf 'macos' ;;
    Linux) printf 'linux' ;;
    *) die "Unsupported operating system: $uname_s" ;;
  esac
}

detect_arch() {
  if [ -n "$ARCH_OVERRIDE" ]; then
    case "$ARCH_OVERRIDE" in
      amd64|x86_64|x64) printf 'amd64' ;;
      arm64|aarch64) printf 'arm64' ;;
      *) die "Unsupported --arch override: $ARCH_OVERRIDE" ;;
    esac
    return 0
  fi
  uname_m="$(uname -m 2>/dev/null || echo x86_64)"
  case "$uname_m" in
    x86_64|amd64) printf 'amd64' ;;
    arm64|aarch64) printf 'arm64' ;;
    *) die "Unsupported CPU architecture: $uname_m" ;;
  esac
}

detect_pkg_mgr() {
  target_os="$1"
  if [ -n "$PKG_MGR_OVERRIDE" ]; then
    printf '%s' "$PKG_MGR_OVERRIDE"
    return 0
  fi
  if [ "$target_os" = "macos" ]; then
    if command -v brew >/dev/null 2>&1; then
      printf 'brew'
    else
      printf 'none'
    fi
    return 0
  fi
  if command -v apt-get >/dev/null 2>&1; then
    printf 'apt'
  elif command -v dnf >/dev/null 2>&1; then
    printf 'dnf'
  elif command -v yum >/dev/null 2>&1; then
    printf 'yum'
  else
    printf 'none'
  fi
}

http_get_to_file() {
  url="$1"
  dest="$2"
  if command -v curl >/dev/null 2>&1; then
    curl -fsSL --connect-timeout 10 --max-time 300 "$url" -o "$dest"
  elif command -v wget >/dev/null 2>&1; then
    wget -qO "$dest" "$url"
  else
    die "Neither curl nor wget is installed."
  fi
}

http_probe_ok() {
  url="$1"
  if command -v curl >/dev/null 2>&1; then
    curl -fsS --connect-timeout 2 --max-time 3 "$url" >/dev/null 2>&1
    return $?
  elif command -v wget >/dev/null 2>&1; then
    wget -q --timeout=3 -O /dev/null "$url" >/dev/null 2>&1
    return $?
  fi
  return 1
}

resolve_mirror() {
  if [ "$MIRROR" != "auto" ]; then
    printf '%s' "$MIRROR"
    return 0
  fi
  if [ "$DRY_RUN" -eq 1 ]; then
    printf 'global'
    return 0
  fi
  if http_probe_ok "https://cn-repo.svc.plus/repo/_health/probe.json"; then
    # Prefer CN if timezone is Asia/Shanghai or CN probe is fast while global is slower
    tz_hint="${TZ:-}"
    if [ -z "$tz_hint" ] && [ -f /etc/timezone ]; then
      tz_hint="$(cat /etc/timezone 2>/dev/null || true)"
    fi
    case "$tz_hint" in
      Asia/Shanghai|Asia/Chongqing|Asia/Harbin|Asia/Urumqi|PRC)
        printf 'cn'
        return 0
        ;;
    esac
  fi
  if http_probe_ok "https://repo.svc.plus/repo/_health/probe.json"; then
    printf 'global'
    return 0
  fi
  if http_probe_ok "https://cn-repo.svc.plus/repo/_health/probe.json"; then
    printf 'cn'
    return 0
  fi
  printf 'global'
}

compute_sha256() {
  file="$1"
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$file" | awk '{print $1}'
  elif command -v shasum >/dev/null 2>&1; then
    shasum -a 256 "$file" | awk '{print $1}'
  elif command -v openssl >/dev/null 2>&1; then
    openssl dgst -sha256 "$file" | awk '{print $NF}'
  else
    die "No SHA-256 tool found (sha256sum, shasum, or openssl)."
  fi
}

verify_file_sha256() {
  file="$1"
  expected="$2"
  if [ "$VERIFY_SHA256" -eq 0 ]; then
    log_warn "Skipping SHA-256 verification (--no-verify)."
    return 0
  fi
  if [ -z "$expected" ]; then
    die "Expected SHA-256 checksum missing for $file (pass --no-verify to bypass)."
  fi
  actual="$(compute_sha256 "$file")"
  if [ "$actual" != "$expected" ]; then
    die "SHA-256 mismatch for $file: expected $expected, got $actual"
  fi
  log_info "SHA-256 verified: $actual"
}

setup_apt_repo() {
  repo_base="$1"
  arch="$2"
  keyring="/usr/share/keyrings/svc-plus-archive-keyring.gpg"
  list_file="/etc/apt/sources.list.d/svc-plus.list"
  line="deb [arch=${arch} signed-by=${keyring}] ${repo_base}/apt stable main"

  log_info "Configuring APT repository (${repo_base}/apt)"
  if [ "$DRY_RUN" -eq 1 ]; then
    printf '[dry-run] curl -fsSL %s/apt/svc-plus-archive-keyring.gpg | sudo tee %s >/dev/null\n' "$repo_base" "$keyring"
    printf '[dry-run] echo "%s" | sudo tee %s >/dev/null\n' "$line" "$list_file"
    printf '[dry-run] sudo apt-get update\n'
    return 0
  fi

  tmp_key="$(mktemp)"
  if ! http_get_to_file "${repo_base}/apt/svc-plus-archive-keyring.gpg" "$tmp_key"; then
    rm -f "$tmp_key"
    die "Failed to download APT keyring from ${repo_base}/apt/svc-plus-archive-keyring.gpg"
  fi
  run_root mkdir -p /usr/share/keyrings /etc/apt/sources.list.d
  run_root cp "$tmp_key" "$keyring"
  run_root chmod 0644 "$keyring"
  rm -f "$tmp_key"

  tmp_list="$(mktemp)"
  printf '%s\n' "$line" > "$tmp_list"
  run_root cp "$tmp_list" "$list_file"
  run_root chmod 0644 "$list_file"
  rm -f "$tmp_list"
  run_root apt-get update
}

setup_rpm_repo() {
  repo_base="$1"
  pkg_mgr="$2"
  repo_file="/etc/yum.repos.d/svc-plus.repo"

  log_info "Configuring YUM/DNF repository (${repo_base}/rpm/svc-plus.repo)"
  if [ "$DRY_RUN" -eq 1 ]; then
    printf '[dry-run] sudo curl -fsSL %s/rpm/svc-plus.repo -o %s\n' "$repo_base" "$repo_file"
    printf '[dry-run] sudo %s makecache\n' "$pkg_mgr"
    return 0
  fi

  tmp_repo="$(mktemp)"
  if ! http_get_to_file "${repo_base}/rpm/svc-plus.repo" "$tmp_repo"; then
    rm -f "$tmp_repo"
    die "Failed to download ${repo_base}/rpm/svc-plus.repo"
  fi
  run_root mkdir -p /etc/yum.repos.d
  run_root cp "$tmp_repo" "$repo_file"
  run_root chmod 0644 "$repo_file"
  rm -f "$tmp_repo"
  run_root "$pkg_mgr" makecache
}

setup_homebrew_tap() {
  tap_url="$1"
  log_info "Configuring Homebrew tap svc-plus/tap -> ${tap_url}"
  if [ "$DRY_RUN" -eq 1 ]; then
    printf '[dry-run] brew tap svc-plus/tap %s\n' "$tap_url"
    return 0
  fi
  brew tap svc-plus/tap "$tap_url"
}

lookup_matrix_asset() {
  # Args: dl_base fallback_dl_base product channel platform fmt_list
  # Outputs: "<url>\t<sha256>\t<filename>"
  dl_base="$1"
  fallback_base="$2"
  prod="$3"
  chan="$4"
  plat="$5"
  fmts="$6"

  if ! command -v python3 >/dev/null 2>&1; then
    die "python3 is required to parse release-matrix.json for direct downloads."
  fi

  tmp_json="$(mktemp)"
  fetched=0
  for url in \
    "${dl_base}/releases/${prod}/release-matrix.json" \
    "${fallback_base}/releases/${prod}/release-matrix.json"; do
    if http_get_to_file "$url" "$tmp_json" 2>/dev/null; then
      fetched=1
      break
    fi
  done

  if [ "$fetched" -eq 0 ]; then
    rm -f "$tmp_json"
    die "Unable to fetch release-matrix.json for ${prod} from ${dl_base} or ${fallback_base}"
  fi

  python3 - "$tmp_json" "$chan" "$plat" "$fmts" "$dl_base" "$fallback_base" "$MIRROR_RESOLVED" <<'PYEOF'
import json
import sys

matrix_path, channel, platform_id, fmts_csv, dl_base, fallback_base, mirror = sys.argv[1:8]
wanted_fmts = [f.strip() for f in fmts_csv.split(",") if f.strip()]

with open(matrix_path, "r", encoding="utf-8") as fh:
    data = json.load(fh)

channels = data.get("channels") or {}
entry = channels.get(channel) or channels.get(data.get("defaultChannel") or "latest") or channels.get("latest") or channels.get("stable")
if not entry:
    sys.exit(2)

cn_synced = bool(((entry.get("mirrors") or {}).get("cn") or {}).get("synced", False))
global_synced = bool(((entry.get("mirrors") or {}).get("global") or {}).get("synced", False))

selected_pkg = None
for plat in entry.get("platforms") or []:
    if plat.get("id") != platform_id:
        continue
    pkgs = plat.get("packages") or []
    for fmt in wanted_fmts:
        for pkg in pkgs:
            if pkg.get("format") == fmt:
                selected_pkg = pkg
                break
        if selected_pkg:
            break
    if selected_pkg:
        break

if not selected_pkg:
    sys.exit(3)

rel_path = selected_pkg.get("path") or ""
github_url = selected_pkg.get("githubUrl") or ""
if mirror == "cn" and cn_synced and rel_path:
    url = f"{dl_base}{rel_path}"
elif global_synced and rel_path:
    url = f"{fallback_base}{rel_path}"
elif github_url:
    url = github_url
else:
    url = f"{dl_base}{rel_path}"

print(f"{url}\t{selected_pkg.get('sha256', '')}\t{selected_pkg.get('name', 'package')}")
PYEOF
  rc=$?
  rm -f "$tmp_json"
  return $rc
}

install_direct_package() {
  target_os="$1"
  fmt="$2"
  dl_base="$3"
  fallback_base="https://dl.svc.plus"

  if [ "$DRY_RUN" -eq 1 ]; then
    log_info "[dry-run] Would resolve ${PRODUCT} (${CHANNEL}, ${target_os}/${fmt}) from ${dl_base}/releases/${PRODUCT}/release-matrix.json"
    printf '[dry-run] Download %s package, verify SHA-256, and install via %s\n' "$fmt" "$target_os"
    return 0
  fi

  asset_info="$(lookup_matrix_asset "$dl_base" "$fallback_base" "$PRODUCT" "$CHANNEL" "$target_os" "$fmt")" || die "No ${fmt} package found for ${PRODUCT} (${target_os}/${CHANNEL})"
  asset_url="$(printf '%s' "$asset_info" | awk -F'\t' '{print $1}')"
  asset_sha="$(printf '%s' "$asset_info" | awk -F'\t' '{print $2}')"
  asset_name="$(printf '%s' "$asset_info" | awk -F'\t' '{print $3}')"

  tmp_dir="$(mktemp -d)"
  dest_file="${tmp_dir}/${asset_name}"
  log_info "Downloading ${asset_url}"
  http_get_to_file "$asset_url" "$dest_file"
  verify_file_sha256 "$dest_file" "$asset_sha"

  case "$fmt" in
    dmg)
      mount_point="$(mktemp -d)"
      hdiutil attach "$dest_file" -nobrowse -quiet -mountpoint "$mount_point"
      app_bundle="$(find "$mount_point" -maxdepth 2 -name '*.app' | head -n 1 || true)"
      if [ -n "$app_bundle" ]; then
        app_base="$(basename "$app_bundle")"
        log_info "Installing ${app_base} to /Applications"
        rm -rf "/Applications/${app_base}"
        cp -R "$app_bundle" "/Applications/${app_base}"
      fi
      hdiutil detach "$mount_point" -quiet || true
      rmdir "$mount_point" 2>/dev/null || true
      ;;
    deb)
      run_root dpkg -i "$dest_file" || run_root apt-get install -f -y
      ;;
    rpm)
      if command -v dnf >/dev/null 2>&1; then
        run_root dnf install -y "$dest_file"
      else
        run_root rpm -Uvh "$dest_file"
      fi
      ;;
    appimage)
      mkdir -p "${HOME}/.local/bin"
      chmod +x "$dest_file"
      mv "$dest_file" "${HOME}/.local/bin/${PRODUCT}.AppImage"
      log_info "Installed AppImage to ${HOME}/.local/bin/${PRODUCT}.AppImage"
      ;;
    tar.gz)
      mkdir -p "${HOME}/.local/share/${PRODUCT}" "${HOME}/.local/bin"
      tar -xzf "$dest_file" -C "${HOME}/.local/share/${PRODUCT}"
      log_info "Extracted ${asset_name} into ${HOME}/.local/share/${PRODUCT}"
      ;;
    binary)
      mkdir -p "${HOME}/.local/bin"
      chmod +x "$dest_file"
      cp "$dest_file" "${HOME}/.local/bin/${PRODUCT}"
      log_info "Installed ${PRODUCT} binary to ${HOME}/.local/bin/${PRODUCT}"
      ;;
    *)
      die "Unsupported direct package format: $fmt"
      ;;
  esac

  rm -rf "$tmp_dir"
}

TARGET_OS="$(detect_os)"
TARGET_ARCH="$(detect_arch)"
PKG_MGR="$(detect_pkg_mgr "$TARGET_OS")"
MIRROR_RESOLVED="$(resolve_mirror)"

if [ "$MIRROR_RESOLVED" = "cn" ]; then
  REPO_BASE="https://cn-repo.svc.plus"
  DL_BASE="https://cn-dl.svc.plus"
  HOMEBREW_TAP_URL="https://cn-repo.svc.plus/homebrew/tap.git"
else
  REPO_BASE="https://repo.svc.plus"
  DL_BASE="https://dl.svc.plus"
  HOMEBREW_TAP_URL="https://github.com/ai-workspace-lab/homebrew-tap.git"
fi

log_info "product=${PRODUCT} channel=${CHANNEL} os=${TARGET_OS} arch=${TARGET_ARCH} pkg_mgr=${PKG_MGR} mirror=${MIRROR_RESOLVED} repo=${REPO_BASE} dl=${DL_BASE}"

if [ "$SETUP_REPO" -eq 1 ]; then
  if [ "$TARGET_OS" = "macos" ]; then
    if [ "$PKG_MGR" = "brew" ]; then
      setup_homebrew_tap "$HOMEBREW_TAP_URL"
    else
      log_warn "Homebrew not detected on macOS; nothing to configure for --setup-repo."
    fi
  else
    case "$PKG_MGR" in
      apt) setup_apt_repo "$REPO_BASE" "$TARGET_ARCH" ;;
      dnf|yum) setup_rpm_repo "$REPO_BASE" "$PKG_MGR" ;;
      *) die "Cannot --setup-repo: no supported package manager (apt, dnf, yum, brew) detected." ;;
    esac
  fi
  if [ "$METHOD" = "auto" ]; then
    log_info "Repository setup complete."
    exit 0
  fi
fi

EFFECTIVE_METHOD="$METHOD"
if [ "$EFFECTIVE_METHOD" = "auto" ]; then
  case "$PRODUCT" in
    xconnect-gateway|xconnect-edge-agent|fluxgate|xworkmate-remote-desktop)
      EFFECTIVE_METHOD="binary"
      ;;
    *)
      if [ "$TARGET_OS" = "macos" ]; then
        if [ "$PKG_MGR" = "brew" ]; then
          EFFECTIVE_METHOD="brew"
        else
          EFFECTIVE_METHOD="dmg"
        fi
      else
        case "$PKG_MGR" in
          apt) EFFECTIVE_METHOD="apt" ;;
          dnf) EFFECTIVE_METHOD="dnf" ;;
          yum) EFFECTIVE_METHOD="yum" ;;
          *) EFFECTIVE_METHOD="tar.gz" ;;
        esac
      fi
      ;;
  esac
fi

case "$EFFECTIVE_METHOD" in
  brew)
    setup_homebrew_tap "$HOMEBREW_TAP_URL"
    run_cmd brew install --cask "svc-plus/tap/${PRODUCT}"
    ;;
  dmg)
    install_direct_package "macos" "dmg" "$DL_BASE"
    ;;
  apt)
    setup_apt_repo "$REPO_BASE" "$TARGET_ARCH"
    run_root apt-get install -y "$PRODUCT"
    ;;
  dnf|yum)
    setup_rpm_repo "$REPO_BASE" "$EFFECTIVE_METHOD"
    run_root "$EFFECTIVE_METHOD" install -y "$PRODUCT"
    ;;
  deb)
    install_direct_package "linux" "deb" "$DL_BASE"
    ;;
  rpm)
    install_direct_package "linux" "rpm" "$DL_BASE"
    ;;
  appimage)
    install_direct_package "linux" "appimage" "$DL_BASE"
    ;;
  tar.gz)
    install_direct_package "linux" "tar.gz" "$DL_BASE"
    ;;
  binary)
    install_direct_package "$TARGET_OS" "binary" "$DL_BASE"
    ;;
  *)
    die "Unsupported install method '$EFFECTIVE_METHOD' for OS '$TARGET_OS'"
    ;;
esac

log_info "Done."
