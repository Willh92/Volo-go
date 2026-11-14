#!/usr/bin/env python3
"""Deterministic standalone plugin packager & catalog verifier for Volo-go.

- Reads `plugins/<id>/manifest.json` and verifies version parity with `plugins/catalog.json`.
- Supports `--print-meta` (GitHub Actions `KEY=VALUE` output) so workflows can check whether
  `plugin-<id>-v<version>` is already published on GitHub Releases BEFORE spending CPU/IO
  building the archive.
- Builds reproducible `<id>/...` `.zip` archives compatible with both Admin Console
  `InstallFromZip` (`POST /api/admin/plugins/install`) and `unzip -d ./plugins`:
  pre-compressed media (`.png`, `.webp`, `.gif`, `.ogg`, etc.) use `ZIP_STORED` (zero double
  compression overhead), while `.moc`, `.mtn`, `.js`, `.css`, `.json`, and `.md` use
  `ZIP_DEFLATED` level 9.
"""

from __future__ import annotations

import argparse
import datetime as dt
import hashlib
import json
import os
import pathlib
import re
import shutil
import subprocess
import sys
import tempfile
import zipfile

STORED_EXTENSIONS = {
    ".png",
    ".webp",
    ".gif",
    ".jpg",
    ".jpeg",
    ".avif",
    ".ogg",
    ".mp3",
    ".m4a",
    ".aac",
    ".opus",
    ".mp4",
    ".webm",
    ".woff",
    ".woff2",
    ".zip",
    ".gz",
}

EXCLUDED_NAMES = {
    ".ds_store",
    "thumbs.db",
    "desktop.ini",
}


def load_and_verify_metadata(repo_root: pathlib.Path, plugin_id: str) -> dict:
    plugin_dir = repo_root / "plugins" / plugin_id
    manifest_path = plugin_dir / "manifest.json"
    catalog_path = repo_root / "plugins" / "catalog.json"

    if not manifest_path.is_file():
        raise FileNotFoundError(f"Missing plugin manifest: {manifest_path}")
    if not catalog_path.is_file():
        raise FileNotFoundError(f"Missing plugin catalog: {catalog_path}")

    manifest = json.loads(manifest_path.read_text(encoding="utf-8"))
    catalog = json.loads(catalog_path.read_text(encoding="utf-8"))

    if manifest.get("id") != plugin_id:
        raise ValueError(
            f"manifest.id mismatch: expected {plugin_id!r}, got {manifest.get('id')!r}"
        )

    version = str(manifest.get("version") or "").strip()
    if not version:
        raise ValueError(f"Empty version in {manifest_path}")

    entry_rel = str(manifest.get("entry") or "main.js").strip()
    entry_path = plugin_dir / entry_rel
    if entry_path.is_file():
        entry_src = entry_path.read_text(encoding="utf-8")
        ver_match = re.search(
            r"""\bPLUGIN_VERSION\s*=\s*(['"])([^'"]+)\1""", entry_src
        )
        if ver_match and ver_match.group(2).strip() != version:
            raise ValueError(
                f"Entry version drift for {plugin_id!r}: "
                f"manifest.json={version!r} != {entry_rel} PLUGIN_VERSION={ver_match.group(2).strip()!r}"
            )

    catalog_entry = None
    for item in catalog.get("plugins", []):
        if isinstance(item, dict) and item.get("id") == plugin_id:
            catalog_entry = item
            break

    if catalog_entry is None:
        raise ValueError(f"Plugin {plugin_id!r} not registered in {catalog_path}")

    cat_version = str(catalog_entry.get("version") or "").strip()
    if cat_version != version:
        raise ValueError(
            f"Catalog version drift for {plugin_id!r}: "
            f"manifest.json={version!r} != catalog.json={cat_version!r}"
        )

    expected_tag = f"plugin-{plugin_id}-v{version}"
    expected_asset = f"plugin-{plugin_id}-v{version}.zip"
    expected_alias = f"plugin-{plugin_id}.zip"

    if catalog_entry.get("release_tag") and catalog_entry["release_tag"] != expected_tag:
        raise ValueError(
            f"catalog.json release_tag={catalog_entry['release_tag']!r} != {expected_tag!r}"
        )
    if (
        catalog_entry.get("release_asset")
        and catalog_entry["release_asset"] != expected_asset
    ):
        raise ValueError(
            f"catalog.json release_asset={catalog_entry['release_asset']!r} != {expected_asset!r}"
        )

    return {
        "id": plugin_id,
        "version": version,
        "min_host_version": str(manifest.get("min_host_version") or "0.1.0"),
        "release_tag": expected_tag,
        "release_asset": expected_asset,
        "release_asset_alias": expected_alias,
        "plugin_dir": plugin_dir,
        "manifest": manifest,
        "catalog_entry": catalog_entry,
    }


def resolve_zip_date_time(epoch_str: str | None) -> tuple[int, int, int, int, int, int]:
    epoch = 1767225600  # 2026-01-01T00:00:00Z fallback
    if epoch_str:
        try:
            parsed = int(epoch_str)
            if parsed >= 315532800:  # >= 1980-01-01 (ZIP format minimum)
                epoch = parsed
        except ValueError:
            pass
    ts = dt.datetime.fromtimestamp(epoch, tz=dt.timezone.utc)
    return (ts.year, ts.month, ts.day, ts.hour, ts.minute, ts.second)


def verify_full_plugin_tree(
    plugin_dir: pathlib.Path, plugin_id: str, manifest: dict
) -> None:
    entry_rel = str(manifest.get("entry") or "main.js").strip()
    if not (plugin_dir / entry_rel).is_file():
        raise FileNotFoundError(
            f"Incomplete checkout for {plugin_id!r}: missing entry {entry_rel}"
        )
    if plugin_id == "kanbanniang":
        required_rels = [
            "LICENSE",
            "README.md",
            "SOURCES.json",
            "assets/live2d.js",
            "assets/widget.js",
            "assets/widget.css",
            "assets/tips.json",
            "assets/model-list.json",
        ]
        for rel in required_rels:
            if not (plugin_dir / rel).is_file():
                raise FileNotFoundError(
                    f"Incomplete kanbanniang tree (sparse-checkout still active?): missing {rel}"
                )
        model_list = json.loads(
            (plugin_dir / "assets" / "model-list.json").read_text(encoding="utf-8")
        )
        models = (
            model_list
            if isinstance(model_list, list)
            else (model_list.get("models") or [])
        )
        if len(models) < 20:
            raise ValueError(
                f"Expected >= 20 model entries in model-list.json, got {len(models)}"
            )


MINIFIABLE_EXTENSIONS = {
    ".js",
    ".css",
    ".json",
}


def resolve_node_binary() -> str:
    env_bin = os.environ.get("NODE_BIN", "").strip()
    if env_bin and pathlib.Path(env_bin).is_file():
        return env_bin
    found = shutil.which("node")
    if found:
        return found
    raise RuntimeError(
        "Node.js is required to minify plugin JS/CSS assets (pass --no-minify to skip)"
    )


def minify_plugin_files(
    repo_root: pathlib.Path,
    plugin_dir: pathlib.Path,
    entry_rel: str,
    files: list[tuple[str, pathlib.Path]],
    tmp_dir: pathlib.Path,
) -> tuple[dict[str, pathlib.Path], int, int]:
    minifier_script = repo_root / ".github" / "scripts" / "minify-plugin-assets.mjs"
    admin_modules = repo_root / "web" / "admin" / "node_modules"
    if not minifier_script.is_file():
        raise FileNotFoundError(f"Missing plugin minifier script: {minifier_script}")
    if not admin_modules.is_dir():
        raise FileNotFoundError(
            f"Missing {admin_modules} (run `npm ci --prefix web/admin` first or pass --no-minify)"
        )

    batch_items = []
    overrides: dict[str, pathlib.Path] = {}
    for idx, (arcname, src_path) in enumerate(files):
        if src_path.suffix.lower() not in MINIFIABLE_EXTENSIONS:
            continue
        rel = src_path.relative_to(plugin_dir).as_posix()
        out_path = tmp_dir / "out" / f"{idx}_{src_path.name}"
        batch_items.append(
            {
                "rel": rel,
                "src": str(src_path),
                "out": str(out_path),
            }
        )
        overrides[arcname] = out_path

    if not batch_items:
        return overrides, 0, 0

    batch_json = tmp_dir / "batch.json"
    batch_json.write_text(
        json.dumps({"entry": entry_rel, "files": batch_items}),
        encoding="utf-8",
    )

    node_bin = resolve_node_binary()
    proc = subprocess.run(
        [node_bin, str(minifier_script), "--batch", str(batch_json)],
        cwd=str(repo_root),
        capture_output=True,
        text=True,
        check=False,
    )
    if proc.returncode != 0:
        err_detail = (proc.stderr or proc.stdout or "").strip()
        raise RuntimeError(
            f"Plugin asset minification failed (exit {proc.returncode}): {err_detail}"
        )

    summary = json.loads(proc.stdout.strip().splitlines()[-1])
    return overrides, int(summary.get("rawBytes", 0)), int(summary.get("minBytes", 0))


def build_plugin_zip(
    repo_root: pathlib.Path,
    plugin_dir: pathlib.Path,
    plugin_id: str,
    manifest: dict,
    out_zip: pathlib.Path,
    zip_dt: tuple[int, int, int, int, int, int],
    minify: bool = True,
) -> tuple[int, int, int, int]:
    files: list[tuple[str, pathlib.Path]] = []
    for p in plugin_dir.rglob("*"):
        if p.is_symlink():
            raise ValueError(f"Symlink forbidden in plugin directory: {p}")
        if not p.is_file():
            continue
        if p.name.lower() in EXCLUDED_NAMES or p.name.startswith("."):
            continue
        rel = p.relative_to(plugin_dir).as_posix()
        files.append((f"{plugin_id}/{rel}", p))

    files.sort(key=lambda item: item[0])
    out_zip.parent.mkdir(parents=True, exist_ok=True)
    if out_zip.exists():
        out_zip.unlink()

    entry_rel = str(manifest.get("entry") or "main.js").strip()
    raw_total = 0
    text_raw_bytes = 0
    text_min_bytes = 0

    with tempfile.TemporaryDirectory(prefix="volo-plugin-min-") as tmp_raw:
        tmp_dir = pathlib.Path(tmp_raw)
        overrides: dict[str, pathlib.Path] = {}
        if minify:
            overrides, text_raw_bytes, text_min_bytes = minify_plugin_files(
                repo_root, plugin_dir, entry_rel, files, tmp_dir
            )

        with zipfile.ZipFile(
            out_zip, mode="w", compression=zipfile.ZIP_DEFLATED, compresslevel=9
        ) as zf:
            for arcname, src_path in files:
                payload_path = overrides.get(arcname, src_path)
                st = payload_path.stat()
                ext = src_path.suffix.lower()
                compress_type = (
                    zipfile.ZIP_STORED
                    if ext in STORED_EXTENSIONS
                    else zipfile.ZIP_DEFLATED
                )
                info = zipfile.ZipInfo(filename=arcname, date_time=zip_dt)
                info.compress_type = compress_type
                info.file_size = st.st_size
                info.create_system = 3  # Unix permissions for consistent Go zip.Mode()
                info.external_attr = 0o100644 << 16
                raw_total += st.st_size
                with payload_path.open("rb") as sf, zf.open(info, mode="w") as zf_out:
                    shutil.copyfileobj(sf, zf_out, 256 * 1024)

    return len(files), raw_total, text_raw_bytes, text_min_bytes


def sha256_file(path: pathlib.Path) -> str:
    h = hashlib.sha256()
    with path.open("rb") as f:
        while True:
            chunk = f.read(1024 * 1024)
            if not chunk:
                break
            h.update(chunk)
    return h.hexdigest()


def write_release_notes(meta: dict, out_notes: pathlib.Path, repo: str) -> None:
    vid = meta["id"]
    ver = meta["version"]
    tag = meta["release_tag"]
    asset = meta["release_asset"]
    alias = meta["release_asset_alias"]
    min_host = meta["min_host_version"]
    cat = meta["catalog_entry"]
    zh_name = (cat.get("name") or {}).get("zh-CN", vid)
    en_name = (cat.get("name") or {}).get("en-US", vid)
    zh_sum = (cat.get("summary") or {}).get("zh-CN", "")
    en_sum = (cat.get("summary") or {}).get("en-US", "")

    lines = [
        f"## {zh_name} `v{ver}` / {en_name} `v{ver}`",
        "",
        f"- **插件 ID / Plugin ID**: `{vid}`",
        f"- **插件版本 / Version**: `v{ver}` (`{tag}`)",
        f"- **最低宿主版本 / Min Host Version**: `v{min_host}`",
        f"- **安装归档 / Release Assets**: `{asset}` (稳定别名 / stable alias: `{alias}`)",
        "",
        "### 中文说明",
        f"- {zh_sum}",
        "- **一键安装 / 升级**：登录 `Volo-go` 管理工作台 -> **「外观与扩展 -> 插件扩展」** -> 上传该 `.zip` 包（升级时开启原位覆盖即可完整保留已有配置与互动计数）；或直接解压至 `./plugins/` 目录。",
        "",
        "### English Notes",
        f"- {en_sum}",
        "- **Install / Upgrade**: Upload `.zip` in `Volo-go` Admin Console -> **Appearance & Extensions -> Plugins** (enable overwrite when upgrading to preserve configuration and KV counters), or extract directly into `./plugins/`.",
    ]
    if repo:
        lines.extend(
            [
                "",
                f"- **Catalog**: [`plugins/catalog.json`](https://github.com/{repo}/blob/main/plugins/catalog.json)",
            ]
        )
    out_notes.write_text("\n".join(lines) + "\n", encoding="utf-8")


def main() -> int:
    parser = argparse.ArgumentParser(description="Package Volo-go standalone plugin")
    parser.add_argument("--plugin", required=True, help="Plugin ID (e.g. kanbanniang)")
    parser.add_argument(
        "--repo-root",
        default=".",
        help="Repository root directory",
    )
    parser.add_argument(
        "--print-meta",
        action="store_true",
        help="Verify manifest/catalog parity and print KEY=VALUE lines without building ZIP",
    )
    parser.add_argument(
        "--output-dir",
        default="dist/plugin-release",
        help="Output directory for built ZIP, alias ZIP, SHA256SUMS.txt, and release-notes.md",
    )
    parser.add_argument(
        "--repo",
        default="",
        help="GitHub repository slug (owner/name) for release notes",
    )
    parser.add_argument(
        "--no-minify",
        action="store_true",
        help="Skip AST minification of plugin .js, .css, and .json assets",
    )
    args = parser.parse_args()

    repo_root = pathlib.Path(args.repo_root).resolve()
    meta = load_and_verify_metadata(repo_root, args.plugin)

    if args.print_meta:
        print(f"plugin_id={meta['id']}")
        print(f"version={meta['version']}")
        print(f"release_tag={meta['release_tag']}")
        print(f"release_asset={meta['release_asset']}")
        print(f"release_asset_alias={meta['release_asset_alias']}")
        print(f"min_host_version={meta['min_host_version']}")
        return 0

    out_dir = pathlib.Path(args.output_dir).resolve()
    out_dir.mkdir(parents=True, exist_ok=True)
    zip_dt = resolve_zip_date_time(os.environ.get("SOURCE_DATE_EPOCH"))

    versioned_zip = out_dir / meta["release_asset"]
    alias_zip = out_dir / meta["release_asset_alias"]
    notes_md = out_dir / "plugin-release-notes.md"
    sums_txt = out_dir / "SHA256SUMS.txt"

    verify_full_plugin_tree(meta["plugin_dir"], meta["id"], meta["manifest"])
    file_count, raw_bytes, text_raw, text_min = build_plugin_zip(
        repo_root,
        meta["plugin_dir"],
        meta["id"],
        meta["manifest"],
        versioned_zip,
        zip_dt,
        minify=not args.no_minify,
    )
    shutil.copyfile(versioned_zip, alias_zip)

    digest = sha256_file(versioned_zip)
    sums_txt.write_text(
        f"{digest}  {versioned_zip.name}\n{digest}  {alias_zip.name}\n",
        encoding="utf-8",
    )
    write_release_notes(meta, notes_md, args.repo)

    zip_bytes = versioned_zip.stat().st_size
    min_info = (
        f", text_minified={text_raw}B->{text_min}B"
        if not args.no_minify and text_raw > 0
        else ""
    )
    print(
        f"Packaged {versioned_zip.name}: {file_count} files{min_info}, "
        f"payload={raw_bytes} bytes -> zip={zip_bytes} bytes ({zip_bytes / (1024 * 1024):.2f} MiB), "
        f"sha256={digest}"
    )
    return 0


if __name__ == "__main__":
    sys.exit(main())
