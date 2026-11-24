#!/usr/bin/env python3
"""Generate WinGet 1.6+ multi-file YAML manifests from installer URL and SHA-256."""

from __future__ import annotations

import argparse
from pathlib import Path
from typing import Any

import yaml


ROOT_DIR = Path(__file__).resolve().parent.parent


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(
        description="Build WinGet 1.6+ (.yaml, .installer.yaml, .locale.en-US.yaml) manifests."
    )
    parser.add_argument("--identifier", required=True, help="PackageIdentifier, e.g. SvcPlus.XConnect")
    parser.add_argument("--version", required=True, help="PackageVersion, e.g. 1.1.9")
    parser.add_argument("--publisher", default="svc.plus", help="Publisher display name")
    parser.add_argument("--package-name", required=True, help="PackageName, e.g. XConnect")
    parser.add_argument("--short-description", default="svc.plus desktop application")
    parser.add_argument("--license", default="Apache-2.0")
    parser.add_argument("--package-url", default="https://github.com/ai-workspace-lab/distribution")
    parser.add_argument("--moniker", default="", help="Optional CLI moniker")
    parser.add_argument("--installer-url", default="", help="Primary installer URL (.msi or .exe)")
    parser.add_argument("--installer-sha256", default="", help="Primary installer SHA-256 hex digest")
    parser.add_argument(
        "--installer-type",
        default="",
        help="WinGet InstallerType (msi, wix, portable, inno, nullsoft, exe); inferred from URL if omitted",
    )
    parser.add_argument("--arch", default="x64", help="Installer architecture (x64, arm64, x86)")
    parser.add_argument(
        "--installer",
        action="append",
        default=[],
        help="Additional installer spec as arch:type:url:sha256 (repeatable)",
    )
    parser.add_argument("--output-dir", required=True, help="Output directory for manifests")
    parser.add_argument(
        "--flat",
        action="store_true",
        help="Write manifest files directly in --output-dir instead of manifests/<first>/<Pub>/<Pkg>/<Ver>/",
    )
    return parser.parse_args()


def infer_installer_type(url: str, explicit: str = "") -> str:
    if explicit:
        return explicit
    lower = url.lower()
    if lower.endswith(".msi"):
        return "msi"
    if "portable" in lower and lower.endswith(".exe"):
        return "portable"
    if lower.endswith(".exe"):
        return "nullsoft"
    if lower.endswith(".zip"):
        return "zip"
    return "exe"


def build_winget_manifests(
    *,
    identifier: str,
    version: str,
    publisher: str,
    package_name: str,
    short_description: str,
    license_id: str,
    package_url: str,
    installers: list[dict[str, str]],
    output_dir: Path,
    moniker: str = "",
    flat: bool = False,
) -> dict[str, Path]:
    if not installers:
        raise ValueError("At least one installer entry is required.")

    clean_version = version.lstrip("v")
    if flat:
        target_dir = output_dir
    else:
        parts = identifier.split(".")
        first_letter = identifier[0].lower()
        target_dir = output_dir / "manifests" / first_letter / Path(*parts) / clean_version

    target_dir.mkdir(parents=True, exist_ok=True)

    manifest_version = "1.6.0"
    version_doc: dict[str, Any] = {
        "PackageIdentifier": identifier,
        "PackageVersion": clean_version,
        "DefaultLocale": "en-US",
        "ManifestType": "version",
        "ManifestVersion": manifest_version,
    }

    formatted_installers: list[dict[str, Any]] = []
    for inst in installers:
        sha = inst["sha256"].upper()
        if len(sha) != 64:
            raise ValueError(f"Invalid SHA-256 length for {inst['url']}: {inst['sha256']}")
        entry: dict[str, Any] = {
            "Architecture": inst.get("arch", "x64"),
            "InstallerType": infer_installer_type(inst["url"], inst.get("type", "")),
            "InstallerUrl": inst["url"],
            "InstallerSha256": sha,
        }
        if entry["InstallerType"] == "portable" and moniker:
            entry["Commands"] = [moniker]
        formatted_installers.append(entry)

    installer_doc: dict[str, Any] = {
        "PackageIdentifier": identifier,
        "PackageVersion": clean_version,
        "ManifestType": "installer",
        "ManifestVersion": manifest_version,
        "Installers": formatted_installers,
    }

    locale_doc: dict[str, Any] = {
        "PackageIdentifier": identifier,
        "PackageVersion": clean_version,
        "PackageLocale": "en-US",
        "Publisher": publisher,
        "PackageName": package_name,
        "PackageUrl": package_url,
        "License": license_id,
        "ShortDescription": short_description,
        "ManifestType": "defaultLocale",
        "ManifestVersion": manifest_version,
    }
    if moniker:
        locale_doc["Moniker"] = moniker

    version_path = target_dir / f"{identifier}.yaml"
    installer_path = target_dir / f"{identifier}.installer.yaml"
    locale_path = target_dir / f"{identifier}.locale.en-US.yaml"

    def write_yaml_manifest(path: Path, schema_name: str, payload: dict[str, Any]) -> None:
        header = (
            f"# yaml-language-server: $schema=https://aka.ms/winget-manifest.{schema_name}.{manifest_version}.schema.json\n"
        )
        body = yaml.safe_dump(payload, sort_keys=False, allow_unicode=True)
        path.write_text(header + body, encoding="utf-8")

    write_yaml_manifest(version_path, "version", version_doc)
    write_yaml_manifest(installer_path, "installer", installer_doc)
    write_yaml_manifest(locale_path, "defaultLocale", locale_doc)

    return {
        "version": version_path,
        "installer": installer_path,
        "locale": locale_path,
    }


def main() -> None:
    args = parse_args()
    installers: list[dict[str, str]] = []
    if args.installer_url:
        if not args.installer_sha256:
            raise SystemExit("--installer-sha256 is required with --installer-url")
        installers.append(
            {
                "arch": args.arch,
                "type": args.installer_type,
                "url": args.installer_url,
                "sha256": args.installer_sha256,
            }
        )
    for spec in args.installer:
        parts = spec.split(":", 3)
        if len(parts) != 4:
            raise SystemExit(f"Invalid --installer spec '{spec}', expected arch:type:url:sha256")
        installers.append({"arch": parts[0], "type": parts[1], "url": parts[2], "sha256": parts[3]})

    written = build_winget_manifests(
        identifier=args.identifier,
        version=args.version,
        publisher=args.publisher,
        package_name=args.package_name,
        short_description=args.short_description,
        license_id=args.license,
        package_url=args.package_url,
        installers=installers,
        output_dir=Path(args.output_dir),
        moniker=args.moniker,
        flat=args.flat,
    )
    for kind, path in written.items():
        print(f"Generated WinGet {kind} manifest: {path}")


if __name__ == "__main__":
    main()
