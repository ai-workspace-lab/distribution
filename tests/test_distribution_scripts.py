#!/usr/bin/env python3
"""Comprehensive tests for ai-workspace-lab/distribution scripts and templates."""

from __future__ import annotations

import gzip
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest

import yaml


REPO_ROOT = Path(__file__).resolve().parent.parent
SCRIPTS_DIR = REPO_ROOT / "scripts"
CATALOG_PATH = REPO_ROOT / "catalog" / "products.yaml"


def make_sample_matrices(matrix_dir: Path) -> None:
    matrix_dir.mkdir(parents=True, exist_ok=True)
    xconnect_matrix = {
        "schemaVersion": 1,
        "product": "xconnect",
        "defaultChannel": "latest",
        "channels": {
            "stable": None,
            "latest": {
                "tag": "release-1.1.9-test",
                "version": "1.1.9",
                "channel": "latest",
                "publishedAt": "2026-11-24T00:00:00Z",
                "mirrors": {
                    "cn": {"synced": True},
                    "global": {"synced": True},
                },
                "platforms": [
                    {
                        "id": "macos",
                        "packages": [
                            {
                                "name": "xconnect.dmg",
                                "format": "dmg",
                                "arch": "arm64",
                                "path": "/releases/xconnect/release-1.1.9-test/xconnect.dmg",
                                "sha256": "a" * 64,
                                "githubUrl": "https://github.com/ai-workspace-xstream/xconnect-app/releases/download/release-1.1.9-test/xconnect.dmg",
                            }
                        ],
                    },
                    {
                        "id": "windows",
                        "packages": [
                            {
                                "name": "xconnect-windows.msi",
                                "format": "msi",
                                "arch": "x64",
                                "path": "/releases/xconnect/release-1.1.9-test/xconnect-windows.msi",
                                "sha256": "b" * 64,
                                "githubUrl": "https://github.com/ai-workspace-xstream/xconnect-app/releases/download/release-1.1.9-test/xconnect-windows.msi",
                            },
                            {
                                "name": "xconnect-windows.zip",
                                "format": "zip",
                                "arch": "x64",
                                "path": "/releases/xconnect/release-1.1.9-test/xconnect-windows.zip",
                                "sha256": "c" * 64,
                                "githubUrl": "https://github.com/ai-workspace-xstream/xconnect-app/releases/download/release-1.1.9-test/xconnect-windows.zip",
                            },
                        ],
                    },
                ],
            },
        },
    }
    xworkmate_matrix = {
        "schemaVersion": 1,
        "product": "xworkmate",
        "defaultChannel": "stable",
        "channels": {
            "stable": {
                "tag": "v1.2.0-build.851",
                "version": "1.2.0",
                "channel": "stable",
                "publishedAt": "2026-11-24T00:00:00Z",
                "mirrors": {
                    "cn": {"synced": False},
                    "global": {"synced": False},
                },
                "platforms": [
                    {
                        "id": "macos",
                        "packages": [
                            {
                                "name": "XWorkmate-1.2.0.dmg",
                                "format": "dmg",
                                "arch": "arm64",
                                "path": "",
                                "sha256": "d" * 64,
                                "githubUrl": "https://github.com/ai-workspace-lab/xworkmate-app/releases/download/v1.2.0-build.851/XWorkmate-1.2.0.dmg",
                            }
                        ],
                    },
                    {
                        "id": "windows",
                        "packages": [
                            {
                                "name": "xworkmate-windows-amd64.zip",
                                "format": "zip",
                                "arch": "x64",
                                "path": "",
                                "sha256": "e" * 64,
                                "githubUrl": "https://github.com/ai-workspace-lab/xworkmate-app/releases/download/v1.2.0-build.851/xworkmate-windows-amd64.zip",
                            }
                        ],
                    },
                ],
            },
            "latest": None,
        },
    }
    (matrix_dir / "xconnect.json").write_text(json.dumps(xconnect_matrix), encoding="utf-8")
    (matrix_dir / "xworkmate.json").write_text(json.dumps(xworkmate_matrix), encoding="utf-8")


class DistributionScriptsTest(unittest.TestCase):
    def test_catalog_yaml_schema(self) -> None:
        catalog = yaml.safe_load(CATALOG_PATH.read_text(encoding="utf-8"))
        self.assertEqual(catalog["mirrors"]["dl"], "https://dl.svc.plus")
        self.assertEqual(catalog["mirrors"]["cn_dl"], "https://cn-dl.svc.plus")
        self.assertEqual(catalog["mirrors"]["repo"], "https://repo.svc.plus")
        self.assertEqual(catalog["mirrors"]["cn_repo"], "https://cn-repo.svc.plus")
        for pid in ("xconnect", "xworkmate"):
            self.assertIn(pid, catalog["products"])
            pkgs = catalog["products"][pid]["packages"]
            for fmt in ("deb", "rpm", "homebrew", "scoop", "winget", "direct"):
                self.assertIn(fmt, pkgs)

    def test_shell_syntax_checks(self) -> None:
        res_sh = subprocess.run(["sh", "-n", str(SCRIPTS_DIR / "install.sh")], capture_output=True, text=True)
        self.assertEqual(res_sh.returncode, 0, res_sh.stderr)

        for script in ("build_apt_repo.sh", "build_rpm_repo.sh", "sync_homebrew_and_scoop.sh"):
            res_bash = subprocess.run(["bash", "-n", str(SCRIPTS_DIR / script)], capture_output=True, text=True)
            self.assertEqual(res_bash.returncode, 0, f"{script}: {res_bash.stderr}")

    def test_install_sh_help_and_dry_runs(self) -> None:
        install_sh = str(SCRIPTS_DIR / "install.sh")

        help_res = subprocess.run(["sh", install_sh, "--help"], capture_output=True, text=True)
        self.assertEqual(help_res.returncode, 0, help_res.stderr)
        self.assertIn("--setup-repo", help_res.stdout)
        self.assertIn("--mirror", help_res.stdout)

        # Debian / APT dry-run (CN mirror + setup-repo)
        deb_res = subprocess.run(
            [
                "sh",
                install_sh,
                "--dry-run",
                "--setup-repo",
                "--mirror",
                "cn",
                "--product",
                "xconnect",
                "--os",
                "linux",
                "--arch",
                "amd64",
                "--pkg-mgr",
                "apt",
            ],
            capture_output=True,
            text=True,
        )
        self.assertEqual(deb_res.returncode, 0, deb_res.stderr)
        self.assertIn("https://cn-repo.svc.plus/apt", deb_res.stdout)
        self.assertIn("/etc/apt/sources.list.d/svc-plus.list", deb_res.stdout)
        self.assertIn("apt-get update", deb_res.stdout)

        # RHEL / DNF dry-run (Global mirror + setup-repo + install)
        rhel_res = subprocess.run(
            [
                "sh",
                install_sh,
                "--dry-run",
                "--setup-repo",
                "--mirror",
                "global",
                "--product",
                "xworkmate",
                "--method",
                "dnf",
                "--os",
                "linux",
                "--arch",
                "arm64",
                "--pkg-mgr",
                "dnf",
            ],
            capture_output=True,
            text=True,
        )
        self.assertEqual(rhel_res.returncode, 0, rhel_res.stderr)
        self.assertIn("https://repo.svc.plus/rpm/svc-plus.repo", rhel_res.stdout)
        self.assertIn("/etc/yum.repos.d/svc-plus.repo", rhel_res.stdout)
        self.assertIn("dnf install -y xworkmate", rhel_res.stdout)

        # macOS / Homebrew dry-run (CN mirror)
        mac_res = subprocess.run(
            [
                "sh",
                install_sh,
                "--dry-run",
                "--mirror",
                "cn",
                "--product",
                "xconnect",
                "--os",
                "macos",
                "--arch",
                "arm64",
                "--pkg-mgr",
                "brew",
            ],
            capture_output=True,
            text=True,
        )
        self.assertEqual(mac_res.returncode, 0, mac_res.stderr)
        self.assertIn("https://cn-repo.svc.plus/homebrew/tap.git", mac_res.stdout)
        self.assertIn("brew install --cask svc-plus/tap/xconnect", mac_res.stdout)

    def test_publish_static_site(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            tmp_path = Path(tmp)
            matrix_dir = tmp_path / "matrices"
            out_dir = tmp_path / "dist"
            make_sample_matrices(matrix_dir)

            res = subprocess.run(
                [
                    sys.executable,
                    str(SCRIPTS_DIR / "publish_static_site.py"),
                    "--catalog",
                    str(CATALOG_PATH),
                    "--matrix-dir",
                    str(matrix_dir),
                    "--output-dir",
                    str(out_dir),
                ],
                capture_output=True,
                text=True,
            )
            self.assertEqual(res.returncode, 0, res.stderr)

            for mirror, repo_host, dl_host in [
                ("global", "repo.svc.plus", "dl.svc.plus"),
                ("cn", "cn-repo.svc.plus", "cn-dl.svc.plus"),
            ]:
                mdir = out_dir / mirror
                for rel in (
                    "index.html",
                    "repo-index.json",
                    "rpm/svc-plus.repo",
                    "repo/_health/probe.json",
                    "install.sh",
                    "install.ps1",
                ):
                    self.assertTrue((mdir / rel).is_file(), f"Missing {mirror}/{rel}")

                html_text = (mdir / "index.html").read_text(encoding="utf-8")
                self.assertIn(repo_host, html_text)
                self.assertIn(dl_host, html_text)

                idx = json.loads((mdir / "repo-index.json").read_text(encoding="utf-8"))
                self.assertEqual(idx["mirror"], mirror)
                self.assertEqual(idx["repoBaseUrl"], f"https://{repo_host}")
                self.assertEqual(idx["dlBaseUrl"], f"https://{dl_host}")
                self.assertEqual(idx["products"]["xconnect"]["latest"]["version"], "1.1.9")
                self.assertEqual(idx["products"]["xworkmate"]["latest"]["version"], "1.2.0")

                repo_file = (mdir / "rpm/svc-plus.repo").read_text(encoding="utf-8")
                self.assertIn(f"baseurl=https://{repo_host}/rpm/stable/$basearch", repo_file)

                probe = json.loads((mdir / "repo/_health/probe.json").read_text(encoding="utf-8"))
                self.assertEqual(probe["status"], "ok")
                self.assertEqual(probe["repoHost"], repo_host)

    def test_build_winget_manifest(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            out_dir = Path(tmp) / "winget"
            sha = "a1b2c3d4" * 8
            res = subprocess.run(
                [
                    sys.executable,
                    str(SCRIPTS_DIR / "build_winget_manifest.py"),
                    "--identifier",
                    "SvcPlus.XConnect",
                    "--version",
                    "v1.1.9",
                    "--publisher",
                    "svc.plus",
                    "--package-name",
                    "XConnect",
                    "--short-description",
                    "Cross-platform secure connectivity client",
                    "--moniker",
                    "xconnect",
                    "--installer-url",
                    "https://dl.svc.plus/releases/xconnect/release-1.1.9/xconnect-windows.msi",
                    "--installer-sha256",
                    sha,
                    "--output-dir",
                    str(out_dir),
                ],
                capture_output=True,
                text=True,
            )
            self.assertEqual(res.returncode, 0, res.stderr)

            base = out_dir / "manifests" / "s" / "SvcPlus" / "XConnect" / "1.1.9"
            ver_yaml = yaml.safe_load((base / "SvcPlus.XConnect.yaml").read_text(encoding="utf-8"))
            inst_yaml = yaml.safe_load((base / "SvcPlus.XConnect.installer.yaml").read_text(encoding="utf-8"))
            loc_yaml = yaml.safe_load((base / "SvcPlus.XConnect.locale.en-US.yaml").read_text(encoding="utf-8"))

            self.assertEqual(ver_yaml["ManifestType"], "version")
            self.assertEqual(ver_yaml["ManifestVersion"], "1.6.0")
            self.assertEqual(ver_yaml["PackageVersion"], "1.1.9")

            self.assertEqual(inst_yaml["ManifestType"], "installer")
            self.assertEqual(inst_yaml["Installers"][0]["InstallerType"], "msi")
            self.assertEqual(inst_yaml["Installers"][0]["InstallerSha256"], sha.upper())

            self.assertEqual(loc_yaml["ManifestType"], "defaultLocale")
            self.assertEqual(loc_yaml["PackageLocale"], "en-US")
            self.assertEqual(loc_yaml["Moniker"], "xconnect")

    def test_sync_homebrew_and_scoop_dry_run(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            tmp_path = Path(tmp)
            matrix_dir = tmp_path / "matrices"
            out_dir = tmp_path / "dist"
            make_sample_matrices(matrix_dir)

            env = dict(os.environ)
            env["PYTHON_BIN"] = sys.executable
            res = subprocess.run(
                [
                    "bash",
                    str(SCRIPTS_DIR / "sync_homebrew_and_scoop.sh"),
                    "--catalog",
                    str(CATALOG_PATH),
                    "--matrix-dir",
                    str(matrix_dir),
                    "--output-dir",
                    str(out_dir),
                    "--offline",
                    "--dry-run",
                ],
                env=env,
                capture_output=True,
                text=True,
            )
            self.assertEqual(res.returncode, 0, res.stderr)

            # Verify Global Casks & Scoop manifests
            global_cask = (out_dir / "global/homebrew/Casks/xconnect.rb").read_text(encoding="utf-8")
            self.assertIn('url "https://dl.svc.plus/releases/xconnect/release-1.1.9-test/xconnect.dmg"', global_cask)
            cn_cask = (out_dir / "cn/homebrew/Casks/xconnect.rb").read_text(encoding="utf-8")
            self.assertIn('url "https://cn-dl.svc.plus/releases/xconnect/release-1.1.9-test/xconnect.dmg"', cn_cask)

            # xworkmate is not synced to CN yet -> must fall back cleanly to GitHub Releases URL
            cn_xwm_cask = (out_dir / "cn/homebrew/Casks/xworkmate.rb").read_text(encoding="utf-8")
            self.assertIn(
                'url "https://github.com/ai-workspace-lab/xworkmate-app/releases/download/v1.2.0-build.851/XWorkmate-1.2.0.dmg"',
                cn_xwm_cask,
            )

            global_scoop = json.loads((out_dir / "global/scoop/bucket/xconnect.json").read_text(encoding="utf-8"))
            self.assertEqual(global_scoop["version"], "1.1.9")
            self.assertEqual(
                global_scoop["architecture"]["64bit"]["url"],
                "https://dl.svc.plus/releases/xconnect/release-1.1.9-test/xconnect-windows.zip",
            )
            cn_scoop = json.loads((out_dir / "cn/scoop/bucket/xconnect.json").read_text(encoding="utf-8"))
            self.assertEqual(
                cn_scoop["architecture"]["64bit"]["url"],
                "https://cn-dl.svc.plus/releases/xconnect/release-1.1.9-test/xconnect-windows.zip",
            )

            # Verify bare Dumb-HTTP git repos (info/refs + objects/info/packs + clonable)
            for mirror in ("global", "cn"):
                for repo_rel in ("homebrew/tap.git", "scoop/bucket.git"):
                    bare = out_dir / mirror / repo_rel
                    self.assertTrue((bare / "HEAD").is_file())
                    self.assertTrue((bare / "info/refs").is_file())
                    self.assertTrue((bare / "objects/info/packs").is_file())

                    clone_target = tmp_path / f"clone-{mirror}-{repo_rel.replace('/', '-')}"
                    clone_res = subprocess.run(
                        ["git", "clone", str(bare), str(clone_target)],
                        capture_output=True,
                        text=True,
                    )
                    self.assertEqual(clone_res.returncode, 0, clone_res.stderr)

    def test_build_apt_and_rpm_repos_dry_run(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            tmp_path = Path(tmp)
            src_debs = tmp_path / "debs"
            src_rpms = tmp_path / "rpms"
            out_dir = tmp_path / "repo"
            src_debs.mkdir()
            src_rpms.mkdir()

            (src_debs / "xconnect-linux-amd64.deb").write_bytes(b"fake-deb-amd64")
            (src_debs / "xworkmate_1.2.0-1_arm64.deb").write_bytes(b"fake-deb-arm64")
            (src_rpms / "xconnect-linux-x86_64.rpm").write_bytes(b"fake-rpm-x86_64")
            (src_rpms / "xworkmate-1.2.0-1.aarch64.rpm").write_bytes(b"fake-rpm-aarch64")

            apt_res = subprocess.run(
                [
                    "bash",
                    str(SCRIPTS_DIR / "build_apt_repo.sh"),
                    "--source-debs",
                    str(src_debs),
                    "--output-dir",
                    str(out_dir),
                    "--dry-run",
                ],
                capture_output=True,
                text=True,
            )
            self.assertEqual(apt_res.returncode, 0, apt_res.stderr)
            self.assertTrue((out_dir / "apt/pool/main/xconnect/xconnect-linux-amd64.deb").is_file())
            self.assertTrue((out_dir / "apt/pool/main/xworkmate/xworkmate_1.2.0-1_arm64.deb").is_file())

            amd64_pkgs = (out_dir / "apt/dists/stable/main/binary-amd64/Packages").read_text(encoding="utf-8")
            self.assertIn("Filename: pool/main/xconnect/xconnect-linux-amd64.deb", amd64_pkgs)
            self.assertTrue((out_dir / "apt/dists/stable/main/binary-amd64/Packages.gz").is_file())
            with gzip.open(out_dir / "apt/dists/stable/main/binary-arm64/Packages.gz", "rt", encoding="utf-8") as gz:
                self.assertIn("pool/main/xworkmate/xworkmate_1.2.0-1_arm64.deb", gz.read())
            self.assertTrue((out_dir / "apt/dists/stable/Release").is_file())
            self.assertTrue((out_dir / "apt/dists/testing/Release").is_file())

            rpm_res = subprocess.run(
                [
                    "bash",
                    str(SCRIPTS_DIR / "build_rpm_repo.sh"),
                    "--source-rpms",
                    str(src_rpms),
                    "--output-dir",
                    str(out_dir),
                    "--mirror-url",
                    "https://cn-repo.svc.plus",
                    "--dry-run",
                ],
                capture_output=True,
                text=True,
            )
            self.assertEqual(rpm_res.returncode, 0, rpm_res.stderr)
            self.assertTrue((out_dir / "rpm/stable/x86_64/xconnect-linux-x86_64.rpm").is_file())
            self.assertTrue((out_dir / "rpm/stable/aarch64/xworkmate-1.2.0-1.aarch64.rpm").is_file())
            self.assertTrue((out_dir / "rpm/stable/x86_64/repodata/repomd.xml").is_file())
            self.assertTrue((out_dir / "rpm/testing/aarch64/repodata/repomd.xml").is_file())
            rpm_repo = (out_dir / "rpm/svc-plus.repo").read_text(encoding="utf-8")
            self.assertIn("https://cn-repo.svc.plus/rpm/stable/$basearch", rpm_repo)


if __name__ == "__main__":
    unittest.main()
