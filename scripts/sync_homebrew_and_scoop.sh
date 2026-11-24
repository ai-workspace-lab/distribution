#!/usr/bin/env bash
set -euo pipefail

# Sync Homebrew Casks and Scoop manifests from release-matrix.json (or GitHub Releases)
# for Global (dl.svc.plus / GitHub) and CN (cn-dl.svc.plus with clean fallback),
# build Dumb-HTTP bare git repos (homebrew/tap.git, scoop/bucket.git), and
# optionally push to ai-workspace-lab/homebrew-tap and ai-workspace-lab/scoop-bucket.

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

CATALOG_PATH="${REPO_ROOT}/catalog/products.yaml"
OUTPUT_DIR="${REPO_ROOT}/dist"
MATRIX_DIR=""
PRODUCTS=""
CHANNEL="latest"
MIRRORS="global,cn"
DRY_RUN=false
PUSH_GITHUB=false
OFFLINE=false

usage() {
  cat <<'EOF'
Usage: sync_homebrew_and_scoop.sh [options]

Options:
  --catalog <path>      Path to catalog/products.yaml (default: catalog/products.yaml)
  --output-dir <dir>    Root output directory (default: dist)
  --matrix-dir <dir>    Optional local directory of <product>.json release matrices
  --products <csv>      Comma-separated product IDs (default: all in catalog)
  --channel <name>      Preferred release channel: latest | stable (default: latest)
  --mirrors <csv>       Comma-separated mirrors: global,cn (default: global,cn)
  --offline             Do not hit remote URLs when local matrix is missing
  --dry-run             Render files and bare Dumb-HTTP git repos without pushing to GitHub
  --push-github         Push Global Casks/Scoop manifests to GitHub tap/bucket repos
  -h, --help            Show this help message
EOF
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --catalog)
      CATALOG_PATH="${2:-}"
      shift 2
      ;;
    --output-dir)
      OUTPUT_DIR="${2:-}"
      shift 2
      ;;
    --matrix-dir)
      MATRIX_DIR="${2:-}"
      shift 2
      ;;
    --products)
      PRODUCTS="${2:-}"
      shift 2
      ;;
    --channel)
      CHANNEL="${2:-}"
      shift 2
      ;;
    --mirrors)
      MIRRORS="${2:-}"
      shift 2
      ;;
    --offline)
      OFFLINE=true
      shift
      ;;
    --dry-run)
      DRY_RUN=true
      shift
      ;;
    --push-github)
      PUSH_GITHUB=true
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

mkdir -p "$OUTPUT_DIR"

PYTHON_BIN="${PYTHON_BIN:-python3}"
"${PYTHON_BIN}" - "$CATALOG_PATH" "$OUTPUT_DIR" "$MATRIX_DIR" "$PRODUCTS" "$CHANNEL" "$MIRRORS" "$OFFLINE" <<'PYEOF'
import fnmatch
import hashlib
import json
import os
from pathlib import Path
import re
import sys
from typing import Any
from urllib.error import URLError
from urllib.request import Request, urlopen

import yaml

catalog_path = Path(sys.argv[1])
output_dir = Path(sys.argv[2])
matrix_dir = Path(sys.argv[3]) if sys.argv[3] else None
products_arg = [p.strip() for p in sys.argv[4].split(",") if p.strip()]
preferred_channel = sys.argv[5]
mirrors = [m.strip() for m in sys.argv[6].split(",") if m.strip()]
offline = sys.argv[7].lower() == "true"

catalog = yaml.safe_load(catalog_path.read_text(encoding="utf-8"))
mirrors_cfg = catalog["mirrors"]
products_cfg = catalog["products"]
selected_products = products_arg or list(products_cfg.keys())


def fetch_json(url: str, headers: dict[str, str] | None = None) -> Any | None:
    if offline:
        return None
    req_headers = {"User-Agent": "svc-plus-distribution/1.0"}
    if headers:
        req_headers.update(headers)
    try:
        req = Request(url, headers=req_headers)
        with urlopen(req, timeout=10) as resp:
            return json.loads(resp.read().decode("utf-8"))
    except (URLError, TimeoutError, json.JSONDecodeError, OSError):
        return None


def fetch_text(url: str) -> str | None:
    if offline:
        return None
    try:
        req = Request(url, headers={"User-Agent": "svc-plus-distribution/1.0"})
        with urlopen(req, timeout=10) as resp:
            return resp.read().decode("utf-8", errors="replace")
    except (URLError, TimeoutError, OSError):
        return None


def fetch_github_release_entry(repo: str, channel: str) -> dict[str, Any] | None:
    gh_headers: dict[str, str] = {"Accept": "application/vnd.github+json"}
    token = os.environ.get("GITHUB_TOKEN") or os.environ.get("HOMEBREW_TAP_TOKEN") or ""
    if token:
        gh_headers["Authorization"] = f"Bearer {token}"

    releases = fetch_json(f"https://api.github.com/repos/{repo}/releases?per_page=10", gh_headers)
    if not isinstance(releases, list) or not releases:
        return None

    chosen = None
    for rel in releases:
        if rel.get("draft"):
            continue
        tag = str(rel.get("tag_name") or "")
        if channel == "stable" and not rel.get("prerelease") and "latest-" not in tag:
            chosen = rel
            break
        if channel == "latest" and ("latest-" in tag or rel.get("prerelease")):
            chosen = rel
            break
    if chosen is None:
        chosen = next((r for r in releases if not r.get("draft")), releases[0])

    tag = str(chosen.get("tag_name") or "v0.0.0")
    ver_match = re.search(r"(\d+\.\d+\.\d+)", tag)
    version = ver_match.group(1) if ver_match else tag.lstrip("v")

    checksum_map: dict[str, str] = {}
    assets = chosen.get("assets") or []
    for asset in assets:
        if asset.get("name") == "SHA256SUMS.txt" and asset.get("browser_download_url"):
            sums_txt = fetch_text(asset["browser_download_url"])
            if sums_txt:
                for line in sums_txt.splitlines():
                    parts = line.strip().split()
                    if len(parts) >= 2:
                        checksum_map[parts[1].lstrip("*")] = parts[0].lower()

    mac_pkgs: list[dict[str, Any]] = []
    win_pkgs: list[dict[str, Any]] = []
    for asset in assets:
        name = str(asset.get("name") or "")
        url = str(asset.get("browser_download_url") or "")
        digest_raw = str(asset.get("digest") or "")
        sha = ""
        if digest_raw.startswith("sha256:"):
            sha = digest_raw.split(":", 1)[1].lower()
        elif name in checksum_map:
            sha = checksum_map[name]

        lower = name.lower()
        if lower.endswith(".dmg"):
            mac_pkgs.append({"name": name, "format": "dmg", "arch": "arm64", "sha256": sha, "githubUrl": url, "path": ""})
        elif lower.endswith(".zip") and "win" in lower:
            win_pkgs.append({"name": name, "format": "zip", "arch": "x64", "sha256": sha, "githubUrl": url, "path": ""})
        elif lower.endswith(".exe"):
            win_pkgs.append({"name": name, "format": "exe", "arch": "x64", "sha256": sha, "githubUrl": url, "path": ""})
        elif lower.endswith(".msi"):
            win_pkgs.append({"name": name, "format": "msi", "arch": "x64", "sha256": sha, "githubUrl": url, "path": ""})

    return {
        "tag": tag,
        "version": version,
        "channel": channel,
        "mirrors": {"cn": {"synced": False}, "global": {"synced": False}},
        "platforms": [
            {"id": "macos", "packages": mac_pkgs},
            {"id": "windows", "packages": win_pkgs},
        ],
    }


def resolve_release_entry(pid: str, pcfg: dict[str, Any]) -> dict[str, Any] | None:
    if matrix_dir is not None:
        for candidate in [matrix_dir / f"{pid}.json", matrix_dir / pid / "release-matrix.json"]:
            if candidate.is_file():
                data = json.loads(candidate.read_text(encoding="utf-8"))
                channels = data.get("channels") or {}
                entry = (
                    channels.get(preferred_channel)
                    or channels.get(pcfg.get("default_channel") or "latest")
                    or channels.get("latest")
                    or channels.get("stable")
                )
                if isinstance(entry, dict):
                    return entry

    for url in [pcfg.get("release_matrix_url"), pcfg.get("cn_release_matrix_url")]:
        if not url:
            continue
        data = fetch_json(str(url))
        if isinstance(data, dict) and isinstance(data.get("channels"), dict):
            channels = data["channels"]
            entry = (
                channels.get(preferred_channel)
                or channels.get(pcfg.get("default_channel") or "latest")
                or channels.get("latest")
                or channels.get("stable")
            )
            if isinstance(entry, dict):
                return entry

    if pcfg.get("repo"):
        return fetch_github_release_entry(str(pcfg["repo"]), preferred_channel)
    return None


def pick_package(packages: list[dict[str, Any]], patterns: list[str], fallback_formats: list[str]) -> dict[str, Any] | None:
    for pattern in patterns:
        for pkg in packages:
            if fnmatch.fnmatch(str(pkg.get("name") or ""), pattern):
                return pkg
    for fmt in fallback_formats:
        for pkg in packages:
            if pkg.get("format") == fmt:
                return pkg
    return None


def resolve_mirror_url(pkg: dict[str, Any], entry: dict[str, Any], mirror: str) -> tuple[str, bool]:
    dl_global = str(mirrors_cfg["dl"]).rstrip("/")
    dl_cn = str(mirrors_cfg["cn_dl"]).rstrip("/")
    rel_path = str(pkg.get("path") or "")
    github_url = str(pkg.get("githubUrl") or "")
    mirrors_state = entry.get("mirrors") or {}
    cn_synced = bool((mirrors_state.get("cn") or {}).get("synced", False))
    global_synced = bool((mirrors_state.get("global") or {}).get("synced", False))

    if mirror == "cn":
        if cn_synced and rel_path:
            return f"{dl_cn}{rel_path}", True
        if global_synced and rel_path:
            return f"{dl_global}{rel_path}", False
        return github_url or f"{dl_global}{rel_path}", False

    if global_synced and rel_path:
        return f"{dl_global}{rel_path}", True
    return github_url or f"{dl_global}{rel_path}", False


def render_cask(pid: str, pcfg: dict[str, Any], entry: dict[str, Any], dmg_pkg: dict[str, Any], mirror: str) -> str:
    hb_cfg = (pcfg.get("packages") or {}).get("homebrew") or {}
    cask_name = hb_cfg.get("cask_name") or pid
    app_name = hb_cfg.get("app_name") or f"{pcfg.get('name', pid)}.app"
    version = str(entry.get("version") or "0.0.0").lstrip("v")
    sha256 = str(dmg_pkg.get("sha256") or "") or ("0" * 64)
    url, _ = resolve_mirror_url(dmg_pkg, entry, mirror)
    zap_paths = hb_cfg.get("zap") or []
    zap_block = ""
    if zap_paths:
        items = "\n".join(f'    "{p}",' for p in zap_paths)
        zap_block = f"\n  zap trash: [\n{items}\n  ]\n"

    return (
        f'cask "{cask_name}" do\n'
        f'  version "{version}"\n'
        f'  sha256 "{sha256}"\n\n'
        f'  url "{url}"\n'
        f'  name "{pcfg.get("name", pid)}"\n'
        f'  desc "{pcfg.get("summary", pid)}"\n'
        f'  homepage "{pcfg.get("homepage", "")}"\n\n'
        f"  depends_on :macos\n\n"
        f'  app "{app_name}"\n'
        f"{zap_block}"
        f"end\n"
    )


def render_scoop_manifest(pid: str, pcfg: dict[str, Any], entry: dict[str, Any], win_pkg: dict[str, Any], mirror: str) -> dict[str, Any]:
    scoop_cfg = (pcfg.get("packages") or {}).get("scoop") or {}
    version = str(entry.get("version") or "0.0.0").lstrip("v")
    sha256 = str(win_pkg.get("sha256") or "") or ("0" * 64)
    url, _ = resolve_mirror_url(win_pkg, entry, mirror)

    manifest: dict[str, Any] = {
        "version": version,
        "description": str(pcfg.get("summary") or pid),
        "homepage": str(pcfg.get("homepage") or ""),
        "license": str(pcfg.get("license") or "Apache-2.0"),
        "architecture": {
            "64bit": {
                "url": url,
                "hash": sha256,
            }
        },
    }
    if win_pkg.get("format") == "exe":
        manifest["bin"] = [[str(win_pkg.get("name")), pid]]
    elif scoop_cfg.get("bin"):
        manifest["bin"] = scoop_cfg["bin"]
    if scoop_cfg.get("shortcuts"):
        manifest["shortcuts"] = scoop_cfg["shortcuts"]
    return manifest


for mirror in mirrors:
    hb_stage = output_dir / mirror / "homebrew" / "stage"
    hb_casks = hb_stage / "Casks"
    hb_casks.mkdir(parents=True, exist_ok=True)
    hb_plain = output_dir / mirror / "homebrew" / "Casks"
    hb_plain.mkdir(parents=True, exist_ok=True)

    sc_stage = output_dir / mirror / "scoop" / "stage"
    sc_bucket = sc_stage / "bucket"
    sc_bucket.mkdir(parents=True, exist_ok=True)
    sc_plain = output_dir / mirror / "scoop" / "bucket"
    sc_plain.mkdir(parents=True, exist_ok=True)

    repo_url = mirrors_cfg["cn_repo"] if mirror == "cn" else mirrors_cfg["repo"]
    (hb_stage / "README.md").write_text(
        f"# svc.plus Homebrew Tap ({mirror})\n\n"
        f"```sh\nbrew tap svc-plus/tap {repo_url}/homebrew/tap.git\n```\n",
        encoding="utf-8",
    )
    (sc_stage / "README.md").write_text(
        f"# svc.plus Scoop Bucket ({mirror})\n\n"
        f"```powershell\nscoop bucket add svc-plus {repo_url}/scoop/bucket.git\n```\n",
        encoding="utf-8",
    )

    for pid in selected_products:
        pcfg = products_cfg.get(pid)
        if not pcfg:
            print(f"[warn] Unknown product '{pid}' in catalog; skipping.", file=sys.stderr)
            continue
        entry = resolve_release_entry(pid, pcfg)
        if not entry:
            print(f"[warn] No release metadata available for '{pid}'; skipping.", file=sys.stderr)
            continue

        platforms = {p.get("id"): p.get("packages") or [] for p in (entry.get("platforms") or []) if isinstance(p, dict)}
        pkgs_cfg = pcfg.get("packages") or {}

        mac_pkgs = platforms.get("macos") or []
        dmg_pkg = pick_package(mac_pkgs, (pkgs_cfg.get("homebrew") or {}).get("asset_patterns") or [], ["dmg"])
        if dmg_pkg:
            cask_text = render_cask(pid, pcfg, entry, dmg_pkg, mirror)
            cask_name = (pkgs_cfg.get("homebrew") or {}).get("cask_name") or pid
            (hb_casks / f"{cask_name}.rb").write_text(cask_text, encoding="utf-8")
            (hb_plain / f"{cask_name}.rb").write_text(cask_text, encoding="utf-8")

        win_pkgs = platforms.get("windows") or []
        scoop_pkg = pick_package(win_pkgs, (pkgs_cfg.get("scoop") or {}).get("asset_patterns") or [], ["zip", "exe", "msi"])
        if scoop_pkg:
            scoop_obj = render_scoop_manifest(pid, pcfg, entry, scoop_pkg, mirror)
            manifest_name = (pkgs_cfg.get("scoop") or {}).get("manifest_name") or pid
            rendered = json.dumps(scoop_obj, indent=2, ensure_ascii=False) + "\n"
            (sc_bucket / f"{manifest_name}.json").write_text(rendered, encoding="utf-8")
            (sc_plain / f"{manifest_name}.json").write_text(rendered, encoding="utf-8")
PYEOF

build_bare_dumb_http_repo() {
  local stage_dir="$1"
  local bare_repo="$2"
  local commit_msg="$3"

  local tmp_work
  tmp_work="$(mktemp -d)"
  cp -R "${stage_dir}/." "${tmp_work}/"

  git -C "$tmp_work" init -b main >/dev/null
  git -C "$tmp_work" config user.name "svc-plus-bot"
  git -C "$tmp_work" config user.email "release@svc.plus"
  git -C "$tmp_work" add -A
  git -C "$tmp_work" commit --allow-empty -m "$commit_msg" >/dev/null

  rm -rf "$bare_repo"
  git clone --bare "$tmp_work" "$bare_repo" >/dev/null 2>&1
  git -C "$bare_repo" symbolic-ref HEAD refs/heads/main
  git -C "$bare_repo" update-server-info
  rm -rf "$tmp_work"
}

IFS=',' read -r -a MIRROR_LIST <<< "$MIRRORS"
for raw_mirror in "${MIRROR_LIST[@]}"; do
  mirror="$(echo "$raw_mirror" | xargs)"
  [[ -n "$mirror" ]] || continue
  hb_stage="${OUTPUT_DIR%/}/${mirror}/homebrew/stage"
  hb_bare="${OUTPUT_DIR%/}/${mirror}/homebrew/tap.git"
  sc_stage="${OUTPUT_DIR%/}/${mirror}/scoop/stage"
  sc_bare="${OUTPUT_DIR%/}/${mirror}/scoop/bucket.git"

  build_bare_dumb_http_repo "$hb_stage" "$hb_bare" "chore(homebrew): sync svc.plus casks (${mirror})"
  build_bare_dumb_http_repo "$sc_stage" "$sc_bare" "chore(scoop): sync svc.plus manifests (${mirror})"

  echo "Built Dumb-HTTP git repos for ${mirror}: ${hb_bare} and ${sc_bare}"
done

push_repo_files() {
  local repo_slug="$1"
  local token="$2"
  local src_subdir="$3"
  local dest_subdir="$4"
  local commit_msg="$5"

  if [[ -z "$token" ]]; then
    echo "Notice: token not set for ${repo_slug}; skipping GitHub push."
    return 0
  fi

  local tmp_clone
  tmp_clone="$(mktemp -d)"
  trap 'rm -rf "$tmp_clone"' RETURN
  git clone --depth 1 "https://x-access-token:${token}@github.com/${repo_slug}.git" "$tmp_clone/repo"
  mkdir -p "$tmp_clone/repo/${dest_subdir}"
  cp -f "${src_subdir}/"* "$tmp_clone/repo/${dest_subdir}/" 2>/dev/null || true

  cd "$tmp_clone/repo"
  if [[ -z "$(git status --porcelain)" ]]; then
    echo "${repo_slug} is already up to date."
    return 0
  fi
  git config user.name "${GIT_AUTHOR_NAME:-github-actions[bot]}"
  git config user.email "${GIT_AUTHOR_EMAIL:-41898282+github-actions[bot]@users.noreply.github.com}"
  git add "${dest_subdir}"
  git commit -m "$commit_msg"
  git push origin HEAD
  echo "Pushed updates to ${repo_slug}."
}

if [[ "$PUSH_GITHUB" == "true" ]]; then
  if [[ "$DRY_RUN" == "true" ]]; then
    echo "[dry-run] Skipping GitHub push for ai-workspace-lab/homebrew-tap and ai-workspace-lab/scoop-bucket."
  else
    hb_token="${HOMEBREW_TAP_TOKEN:-}"
    sc_token="${SCOOP_BUCKET_TOKEN:-}"
    push_repo_files "ai-workspace-lab/homebrew-tap" "$hb_token" "${OUTPUT_DIR%/}/global/homebrew/Casks" "Casks" "chore(cask): sync svc.plus casks"
    push_repo_files "ai-workspace-lab/scoop-bucket" "$sc_token" "${OUTPUT_DIR%/}/global/scoop/bucket" "bucket" "chore(scoop): sync svc.plus manifests"
  fi
fi

# Clean staging worktrees while keeping plain Casks/bucket and bare .git repos
for raw_mirror in "${MIRROR_LIST[@]}"; do
  mirror="$(echo "$raw_mirror" | xargs)"
  [[ -n "$mirror" ]] || continue
  rm -rf "${OUTPUT_DIR%/}/${mirror}/homebrew/stage" "${OUTPUT_DIR%/}/${mirror}/scoop/stage"
done
