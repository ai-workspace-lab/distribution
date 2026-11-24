#!/usr/bin/env python3
"""Render mirror-specific static repository sites for repo.svc.plus and cn-repo.svc.plus."""

from __future__ import annotations

import argparse
from datetime import datetime, timezone
import html
import json
from pathlib import Path
import shutil
from typing import Any
from urllib.error import URLError
from urllib.request import Request, urlopen

import yaml


ROOT_DIR = Path(__file__).resolve().parent.parent


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(
        description="Render static repo sites into dist/{global,cn}/ from catalog/products.yaml."
    )
    parser.add_argument(
        "--catalog",
        default=str(ROOT_DIR / "catalog" / "products.yaml"),
        help="Path to catalog/products.yaml",
    )
    parser.add_argument(
        "--output-dir",
        default=str(ROOT_DIR / "dist"),
        help="Root output directory (mirror subdirectories global/ and cn/ are created here)",
    )
    parser.add_argument(
        "--mirrors",
        default="global,cn",
        help="Comma-separated list of mirrors to render (global,cn)",
    )
    parser.add_argument(
        "--matrix-dir",
        default="",
        help="Optional directory containing local <product>.json or <product>/release-matrix.json files",
    )
    parser.add_argument(
        "--fetch-matrices",
        action="store_true",
        help="Fetch live release-matrix.json files from mirror dl endpoints",
    )
    return parser.parse_args()


def load_catalog(path: Path) -> dict[str, Any]:
    data = yaml.safe_load(path.read_text(encoding="utf-8"))
    if not isinstance(data, dict) or "products" not in data or "mirrors" not in data:
        raise ValueError(f"Invalid catalog format in {path}")
    return data


def load_matrix_for_product(
    product_id: str,
    product_cfg: dict[str, Any],
    matrix_dir: Path | None,
    fetch_matrices: bool,
) -> dict[str, Any] | None:
    if matrix_dir is not None:
        candidates = [
            matrix_dir / f"{product_id}.json",
            matrix_dir / product_id / "release-matrix.json",
        ]
        for candidate in candidates:
            if candidate.is_file():
                return json.loads(candidate.read_text(encoding="utf-8"))

    if not fetch_matrices:
        return None

    urls = [
        product_cfg.get("release_matrix_url"),
        product_cfg.get("cn_release_matrix_url"),
    ]
    for url in urls:
        if not url:
            continue
        try:
            req = Request(str(url), headers={"User-Agent": "svc-plus-distribution/1.0"})
            with urlopen(req, timeout=8) as resp:
                return json.loads(resp.read().decode("utf-8"))
        except (URLError, TimeoutError, json.JSONDecodeError, OSError):
            continue
    return None


def summarize_product_matrix(matrix: dict[str, Any] | None) -> dict[str, Any]:
    if not matrix:
        return {"version": None, "channel": None, "tag": None, "platforms": []}
    channels = matrix.get("channels") or {}
    default_ch = matrix.get("defaultChannel") or "latest"
    entry = channels.get(default_ch) or channels.get("stable") or channels.get("latest")
    if not isinstance(entry, dict):
        return {"version": None, "channel": None, "tag": None, "platforms": []}
    platforms = [
        p.get("id")
        for p in (entry.get("platforms") or [])
        if isinstance(p, dict) and p.get("packages")
    ]
    return {
        "version": entry.get("version"),
        "channel": entry.get("channel"),
        "tag": entry.get("tag"),
        "publishedAt": entry.get("publishedAt"),
        "platforms": platforms,
    }


def render_rpm_repo_file(mirror_name: str, repo_base: str) -> str:
    label_suffix = " (CN Mirror)" if mirror_name == "cn" else ""
    return (
        f"[svc-plus]\n"
        f"name=svc.plus Repository{label_suffix} - Stable\n"
        f"baseurl={repo_base}/rpm/stable/$basearch\n"
        f"enabled=1\n"
        f"gpgcheck=1\n"
        f"repo_gpgcheck=1\n"
        f"gpgkey={repo_base}/rpm/RPM-GPG-KEY-svc-plus\n"
        f"\n"
        f"[svc-plus-testing]\n"
        f"name=svc.plus Repository{label_suffix} - Testing\n"
        f"baseurl={repo_base}/rpm/testing/$basearch\n"
        f"enabled=0\n"
        f"gpgcheck=1\n"
        f"repo_gpgcheck=1\n"
        f"gpgkey={repo_base}/rpm/RPM-GPG-KEY-svc-plus\n"
    )


def render_apt_list_file(repo_base: str) -> str:
    return (
        f"deb [signed-by=/usr/share/keyrings/svc-plus-archive-keyring.gpg] "
        f"{repo_base}/apt stable main\n"
    )


def render_index_html(
    mirror_name: str,
    repo_base: str,
    dl_base: str,
    region_cfg: dict[str, Any],
    products: dict[str, Any],
    summaries: dict[str, dict[str, Any]],
    generated_at: str,
) -> str:
    mirror_label = html.escape(str(region_cfg.get("label") or mirror_name.upper()))
    repo_host = repo_base.removeprefix("https://").removeprefix("http://")
    dl_host = dl_base.removeprefix("https://").removeprefix("http://")
    homebrew_tap = str(region_cfg.get("homebrew_tap_url") or f"{repo_base}/homebrew/tap.git")
    scoop_bucket = str(region_cfg.get("scoop_bucket_url") or f"{repo_base}/scoop/bucket.git")

    product_cards: list[str] = []
    for pid, pcfg in products.items():
        name = html.escape(str(pcfg.get("name") or pid))
        summary = html.escape(str(pcfg.get("summary") or ""))
        repo_slug = html.escape(str(pcfg.get("repo") or ""))
        homepage = html.escape(str(pcfg.get("homepage") or f"https://github.com/{repo_slug}"))
        platforms = ", ".join(str(p) for p in (pcfg.get("platforms") or []))
        info = summaries.get(pid) or {}
        version = info.get("version") or "pending-sync"
        tag = info.get("tag") or "-"
        product_cards.append(
            f"""      <article class="card">
        <div class="card-head">
          <h3>{name}</h3>
          <span class="badge">{html.escape(str(version))}</span>
        </div>
        <p>{summary}</p>
        <p class="meta"><strong>Repo:</strong> <a href="{homepage}">{repo_slug}</a></p>
        <p class="meta"><strong>Platforms:</strong> {html.escape(platforms)}</p>
        <p class="meta"><strong>Tag:</strong> <code>{html.escape(str(tag))}</code></p>
        <pre><code>curl -fsSL {html.escape(repo_base)}/install.sh | sh -s -- --product {html.escape(pid)}</code></pre>
      </article>"""
        )

    cards_html = "\n".join(product_cards)
    mirror_flag = " --mirror cn" if mirror_name == "cn" else ""
    ps_mirror_flag = " -Mirror cn" if mirror_name == "cn" else ""

    return f"""<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="utf-8" />
  <meta name="viewport" content="width=device-width, initial-scale=1" />
  <title>svc.plus Package Repository ({html.escape(repo_host)})</title>
  <style>
    :root {{ color-scheme: dark light; --bg: #0b1020; --card: #121a2f; --fg: #e6edf7; --muted: #9aa8c7; --accent: #5da9ff; --border: #243252; }}
    body {{ margin: 0; font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, sans-serif; background: var(--bg); color: var(--fg); line-height: 1.55; }}
    .wrap {{ max-width: 1040px; margin: 0 auto; padding: 2.5rem 1.25rem 4rem; }}
    header {{ border-bottom: 1px solid var(--border); padding-bottom: 1.5rem; margin-bottom: 2rem; }}
    h1 {{ margin: 0 0 0.4rem; font-size: 1.9rem; }}
    .sub {{ color: var(--muted); margin: 0; }}
    .grid {{ display: grid; grid-template-columns: repeat(auto-fit, minmax(290px, 1fr)); gap: 1rem; margin: 1.5rem 0 2rem; }}
    .card {{ background: var(--card); border: 1px solid var(--border); border-radius: 10px; padding: 1.15rem 1.25rem; }}
    .card-head {{ display: flex; align-items: center; justify-content: space-between; gap: 0.75rem; }}
    .card-head h3 {{ margin: 0; }}
    .badge {{ background: #1c2c4d; color: var(--accent); border: 1px solid var(--border); border-radius: 999px; padding: 0.15rem 0.6rem; font-size: 0.8rem; }}
    .meta {{ color: var(--muted); font-size: 0.9rem; margin: 0.35rem 0; }}
    a {{ color: var(--accent); text-decoration: none; }}
    a:hover {{ text-decoration: underline; }}
    pre {{ background: #070b16; border: 1px solid var(--border); border-radius: 8px; padding: 0.75rem 0.9rem; overflow-x: auto; font-size: 0.86rem; }}
    code {{ font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace; }}
    footer {{ margin-top: 2.5rem; padding-top: 1rem; border-top: 1px solid var(--border); color: var(--muted); font-size: 0.85rem; }}
  </style>
</head>
<body>
  <main class="wrap">
    <header>
      <h1>svc.plus Package Repository</h1>
      <p class="sub">Mirror: <strong>{mirror_label}</strong> &middot; Repo: <code>{html.escape(repo_host)}</code> &middot; Downloads: <code>{html.escape(dl_host)}</code></p>
    </header>

    <section>
      <h2>Quick Install</h2>
      <p><strong>macOS / Linux:</strong></p>
      <pre><code>curl -fsSL {html.escape(repo_base)}/install.sh | sh -s --{mirror_flag} --product xconnect</code></pre>
      <p><strong>Windows (PowerShell):</strong></p>
      <pre><code>&amp; ([scriptblock]::Create((irm {html.escape(repo_base)}/install.ps1))) -Product xconnect{ps_mirror_flag}</code></pre>
    </section>

    <section>
      <h2>Package Managers</h2>
      <div class="grid">
        <div class="card">
          <h3>APT (Debian / Ubuntu)</h3>
          <pre><code>curl -fsSL {html.escape(repo_base)}/apt/svc-plus-archive-keyring.gpg | sudo tee /usr/share/keyrings/svc-plus-archive-keyring.gpg &gt;/dev/null
echo "deb [arch=$(dpkg --print-architecture) signed-by=/usr/share/keyrings/svc-plus-archive-keyring.gpg] {html.escape(repo_base)}/apt stable main" | sudo tee /etc/apt/sources.list.d/svc-plus.list
sudo apt-get update &amp;&amp; sudo apt-get install -y xconnect</code></pre>
        </div>
        <div class="card">
          <h3>YUM / DNF (RHEL / Fedora)</h3>
          <pre><code>sudo curl -fsSL {html.escape(repo_base)}/rpm/svc-plus.repo -o /etc/yum.repos.d/svc-plus.repo
sudo dnf install -y xconnect</code></pre>
        </div>
        <div class="card">
          <h3>Homebrew (macOS)</h3>
          <pre><code>brew tap svc-plus/tap {html.escape(homebrew_tap)}
brew install --cask svc-plus/tap/xconnect</code></pre>
        </div>
        <div class="card">
          <h3>Scoop (Windows)</h3>
          <pre><code>scoop bucket add svc-plus {html.escape(scoop_bucket)}
scoop install svc-plus/xconnect</code></pre>
        </div>
      </div>
    </section>

    <section>
      <h2>Catalog Products</h2>
      <div class="grid">
{cards_html}
      </div>
    </section>

    <footer>
      Generated at <code>{html.escape(generated_at)}</code> &middot; <a href="./repo-index.json">repo-index.json</a> &middot; <a href="./repo/_health/probe.json">health probe</a>
    </footer>
  </main>
</body>
</html>
"""


def publish_static_site(
    catalog_path: Path,
    output_dir: Path,
    mirrors: list[str],
    matrix_dir: Path | None = None,
    fetch_matrices: bool = False,
) -> dict[str, Path]:
    catalog = load_catalog(catalog_path)
    mirrors_cfg = catalog["mirrors"]
    regions_cfg = mirrors_cfg.get("regions") or {}
    products = catalog["products"]
    generated_at = datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")

    summaries: dict[str, dict[str, Any]] = {}
    for pid, pcfg in products.items():
        matrix = load_matrix_for_product(pid, pcfg, matrix_dir, fetch_matrices)
        summaries[pid] = summarize_product_matrix(matrix)

    install_sh = ROOT_DIR / "scripts" / "install.sh"
    install_ps1 = ROOT_DIR / "scripts" / "install.ps1"

    rendered_dirs: dict[str, Path] = {}
    for mirror_name in mirrors:
        if mirror_name == "global":
            repo_base = str(mirrors_cfg["repo"]).rstrip("/")
            dl_base = str(mirrors_cfg["dl"]).rstrip("/")
        elif mirror_name == "cn":
            repo_base = str(mirrors_cfg["cn_repo"]).rstrip("/")
            dl_base = str(mirrors_cfg["cn_dl"]).rstrip("/")
        else:
            raise ValueError(f"Unsupported mirror: {mirror_name}")

        region_cfg = regions_cfg.get(mirror_name) or {}
        mirror_dir = output_dir / mirror_name
        (mirror_dir / "rpm").mkdir(parents=True, exist_ok=True)
        (mirror_dir / "apt").mkdir(parents=True, exist_ok=True)
        (mirror_dir / "repo" / "_health").mkdir(parents=True, exist_ok=True)

        index_html = render_index_html(
            mirror_name=mirror_name,
            repo_base=repo_base,
            dl_base=dl_base,
            region_cfg=region_cfg,
            products=products,
            summaries=summaries,
            generated_at=generated_at,
        )
        (mirror_dir / "index.html").write_text(index_html, encoding="utf-8")

        repo_index = {
            "schemaVersion": 1,
            "mirror": mirror_name,
            "repoBaseUrl": repo_base,
            "dlBaseUrl": dl_base,
            "generatedAt": generated_at,
            "endpoints": {
                "installSh": f"{repo_base}/install.sh",
                "installPs1": f"{repo_base}/install.ps1",
                "apt": f"{repo_base}/apt",
                "aptKeyring": f"{repo_base}/apt/svc-plus-archive-keyring.gpg",
                "rpm": f"{repo_base}/rpm/stable",
                "rpmRepoFile": f"{repo_base}/rpm/svc-plus.repo",
                "homebrewTap": str(region_cfg.get("homebrew_tap_http") or f"{repo_base}/homebrew/tap.git"),
                "scoopBucket": str(region_cfg.get("scoop_bucket_http") or f"{repo_base}/scoop/bucket.git"),
                "winget": f"{repo_base}/winget",
                "health": f"{repo_base}/repo/_health/probe.json",
            },
            "products": {
                pid: {
                    "id": pid,
                    "name": pcfg.get("name"),
                    "repo": pcfg.get("repo"),
                    "category": pcfg.get("category"),
                    "platforms": pcfg.get("platforms", []),
                    "defaultChannel": pcfg.get("default_channel", "latest"),
                    "releaseMatrixUrl": f"{dl_base}/releases/{pid}/release-matrix.json",
                    "latest": summaries.get(pid),
                    "packages": pcfg.get("packages", {}),
                }
                for pid, pcfg in products.items()
            },
        }
        (mirror_dir / "repo-index.json").write_text(
            json.dumps(repo_index, indent=2, ensure_ascii=False) + "\n",
            encoding="utf-8",
        )

        (mirror_dir / "rpm" / "svc-plus.repo").write_text(
            render_rpm_repo_file(mirror_name, repo_base),
            encoding="utf-8",
        )
        (mirror_dir / "apt" / "svc-plus.list").write_text(
            render_apt_list_file(repo_base),
            encoding="utf-8",
        )

        probe_payload = {
            "status": "ok",
            "mirror": mirror_name,
            "repoHost": repo_base.removeprefix("https://").removeprefix("http://"),
            "dlHost": dl_base.removeprefix("https://").removeprefix("http://"),
            "generatedAt": generated_at,
            "productsCount": len(products),
        }
        (mirror_dir / "repo" / "_health" / "probe.json").write_text(
            json.dumps(probe_payload, indent=2) + "\n",
            encoding="utf-8",
        )

        if install_sh.is_file():
            shutil.copy2(install_sh, mirror_dir / "install.sh")
        if install_ps1.is_file():
            shutil.copy2(install_ps1, mirror_dir / "install.ps1")

        rendered_dirs[mirror_name] = mirror_dir

    return rendered_dirs


def main() -> None:
    args = parse_args()
    mirrors = [m.strip() for m in args.mirrors.split(",") if m.strip()]
    matrix_dir = Path(args.matrix_dir) if args.matrix_dir else None
    rendered = publish_static_site(
        catalog_path=Path(args.catalog),
        output_dir=Path(args.output_dir),
        mirrors=mirrors,
        matrix_dir=matrix_dir,
        fetch_matrices=args.fetch_matrices,
    )
    for mirror_name, path in rendered.items():
        print(f"Published static site for {mirror_name}: {path}")


if __name__ == "__main__":
    main()
