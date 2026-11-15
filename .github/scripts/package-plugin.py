#!/usr/bin/env python3
"""Deterministic official extensions (plugins, themes & locales) packager & catalog verifier for Volo-go.

- Supports single-item packaging (`--plugin <id>`, `--theme <id>`, `--locale <code>`) and
  unified bundle packaging (`--bundle`) so all official standalone `.zip` archives are consolidated
  into ONE unified `extensions-v<version>` GitHub Release instead of scattered per-item releases.
- Supports `--verify-all-meta` and `--print-bundle-meta` to enforce 1-to-1 directory/catalog parity,
  unified `extensions-v*` release tags, valid `manifest.update` configs, and release asset deduplication.
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

MINIFIABLE_EXTENSIONS = {
    ".js",
    ".css",
    ".json",
}

VALID_UPDATE_STRATEGIES = {
    "volo-catalog",
    "manifest-json",
    "github-release",
    "hook",
}


def load_and_verify_metadata(repo_root: pathlib.Path, kind: str, ext_id: str) -> dict:
    if kind == "plugin":
        ext_dir = repo_root / "plugins" / ext_id
        manifest_path = ext_dir / "manifest.json"
        catalog_path = repo_root / "plugins" / "catalog.json"
        catalog_key = "plugins"
        prefix = "plugin"
    elif kind == "theme":
        ext_dir = repo_root / "themes" / ext_id
        manifest_path = ext_dir / "theme.json"
        catalog_path = repo_root / "themes" / "catalog.json"
        catalog_key = "themes"
        prefix = "theme"
    elif kind == "locale":
        ext_dir = repo_root / "locales"
        manifest_path = ext_dir / f"{ext_id}.json"
        catalog_path = repo_root / "locales" / "catalog.json"
        catalog_key = "items"
        prefix = "locale"
    else:
        raise ValueError(f"Unsupported extension kind: {kind!r}")

    if not manifest_path.is_file():
        raise FileNotFoundError(f"Missing {kind} manifest/source: {manifest_path}")
    if not catalog_path.is_file():
        raise FileNotFoundError(f"Missing {kind} catalog: {catalog_path}")

    manifest = json.loads(manifest_path.read_text(encoding="utf-8"))
    catalog = json.loads(catalog_path.read_text(encoding="utf-8"))

    if kind == "locale":
        meta_code = str(manifest.get("meta.code") or ext_id).strip()
        if meta_code != ext_id:
            raise ValueError(
                f"{manifest_path.name} meta.code mismatch: expected {ext_id!r}, got {meta_code!r}"
            )
        version = str(manifest.get("meta.version") or "1.0.0").strip()
        mf_min_host = str(manifest.get("meta.min_host_version") or "").strip()
    else:
        if manifest.get("id") != ext_id:
            raise ValueError(
                f"{manifest_path.name} id mismatch: expected {ext_id!r}, got {manifest.get('id')!r}"
            )
        version = str(manifest.get("version") or "").strip()
        if not version:
            raise ValueError(f"Empty version in {manifest_path}")
        mf_min_host = str(manifest.get("min_host_version") or "").strip()

        upd = manifest.get("update")
        if not isinstance(upd, dict) or str(upd.get("strategy") or "").strip() not in VALID_UPDATE_STRATEGIES:
            raise ValueError(
                f"{manifest_path} must declare a valid 'update' block with strategy in {sorted(VALID_UPDATE_STRATEGIES)}"
            )
        if kind == "theme" and str(upd.get("strategy") or "").strip() == "hook":
            raise ValueError(f"{manifest_path}: theme update strategy cannot be 'hook'")

    if kind == "plugin":
        entry_rel = str(manifest.get("entry") or "main.js").strip()
        entry_path = ext_dir / entry_rel
        if entry_path.is_file():
            entry_src = entry_path.read_text(encoding="utf-8")
            ver_match = re.search(
                r"""\bPLUGIN_VERSION\s*=\s*(['"])([^'"]+)\1""", entry_src
            )
            if ver_match and ver_match.group(2).strip() != version:
                raise ValueError(
                    f"Entry version drift for {ext_id!r}: "
                    f"manifest.json={version!r} != {entry_rel} PLUGIN_VERSION={ver_match.group(2).strip()!r}"
                )

    catalog_entry = None
    for item in catalog.get(catalog_key, []):
        if isinstance(item, dict) and item.get("id") == ext_id:
            catalog_entry = item
            break

    if catalog_entry is None:
        raise ValueError(f"{kind.capitalize()} {ext_id!r} not registered in {catalog_path}")

    cat_version = str(catalog_entry.get("version") or "").strip()
    if cat_version != version:
        raise ValueError(
            f"Catalog version drift for {ext_id!r}: "
            f"{manifest_path.name}={version!r} != catalog.json={cat_version!r}"
        )

    cat_min_host = str(catalog_entry.get("min_host_version") or "").strip()
    if mf_min_host and cat_min_host and mf_min_host != cat_min_host:
        raise ValueError(
            f"Catalog min_host_version drift for {ext_id!r}: "
            f"{manifest_path.name}={mf_min_host!r} != catalog.json={cat_min_host!r}"
        )

    expected_asset = f"{prefix}-{ext_id}-v{version}.zip"
    expected_alias = f"{prefix}-{ext_id}.zip"

    cat_top_tag = str(catalog.get("release_tag") or "").strip()
    entry_tag = str(catalog_entry.get("release_tag") or cat_top_tag).strip()
    if not entry_tag or not entry_tag.startswith("extensions-v"):
        raise ValueError(
            f"{catalog_path.name} entry {ext_id!r} must use unified 'extensions-v<ver>' release_tag (got {entry_tag!r})"
        )
    if cat_top_tag and entry_tag != cat_top_tag:
        raise ValueError(
            f"{catalog_path.name} entry {ext_id!r} release_tag={entry_tag!r} != top-level {cat_top_tag!r}"
        )

    if catalog_entry.get("release_asset") != expected_asset:
        raise ValueError(
            f"{catalog_path.name} release_asset={catalog_entry.get('release_asset')!r} != {expected_asset!r}"
        )
    if (
        catalog_entry.get("release_asset_alias")
        and catalog_entry["release_asset_alias"] != expected_alias
    ):
        raise ValueError(
            f"{catalog_path.name} release_asset_alias={catalog_entry['release_asset_alias']!r} != {expected_alias!r}"
        )

    dl_url = str(catalog_entry.get("download_url") or "").strip()
    expected_dl_suffix = f"/releases/download/{entry_tag}/{expected_asset}"
    if not dl_url.endswith(expected_dl_suffix):
        raise ValueError(
            f"{catalog_path.name} download_url={dl_url!r} must end with {expected_dl_suffix!r}"
        )

    cs_url = str(catalog_entry.get("checksum_url") or "").strip()
    expected_cs_suffix = f"/releases/download/{entry_tag}/SHA256SUMS.txt"
    if not cs_url.endswith(expected_cs_suffix):
        raise ValueError(
            f"{catalog_path.name} checksum_url={cs_url!r} must end with {expected_cs_suffix!r}"
        )

    rel_path = f"locales/{ext_id}.json" if kind == "locale" else f"{kind}s/{ext_id}"
    return {
        "kind": kind,
        "id": ext_id,
        "version": version,
        "min_host_version": str(mf_min_host or cat_min_host or "0.1.0"),
        "release_tag": entry_tag,
        "release_asset": expected_asset,
        "release_asset_alias": expected_alias,
        "rel_path": rel_path,
        "ext_dir": ext_dir,
        "manifest_path": manifest_path,
        "manifest": manifest,
        "catalog_entry": catalog_entry,
    }


def verify_all_extensions(repo_root: pathlib.Path) -> tuple[str, list[dict]]:
    # 1. Discover on-disk plugins, external themes, and external locales
    plugins_root = repo_root / "plugins"
    themes_root = repo_root / "themes"
    locales_root = repo_root / "locales"

    disk_plugins = sorted(
        d.name
        for d in plugins_root.iterdir()
        if d.is_dir() and not d.name.startswith(".") and (d / "manifest.json").is_file()
    )
    disk_themes = sorted(
        d.name
        for d in themes_root.iterdir()
        if d.is_dir() and not d.name.startswith(".") and (d / "theme.json").is_file()
    )
    disk_locales = sorted(
        f.stem
        for f in locales_root.glob("*.json")
        if f.is_file() and f.name.lower() != "catalog.json" and not f.name.startswith(".")
    )

    p_cat = json.loads((plugins_root / "catalog.json").read_text(encoding="utf-8"))
    t_cat = json.loads((themes_root / "catalog.json").read_text(encoding="utf-8"))
    l_cat = json.loads((locales_root / "catalog.json").read_text(encoding="utf-8"))

    cat_plugins = sorted(str(x.get("id") or "") for x in p_cat.get("plugins", []) if isinstance(x, dict))
    cat_themes = sorted(str(x.get("id") or "") for x in t_cat.get("themes", []) if isinstance(x, dict))
    cat_locales = sorted(str(x.get("id") or "") for x in l_cat.get("items", []) if isinstance(x, dict))

    if disk_plugins != cat_plugins:
        raise ValueError(f"Plugin directory/catalog mismatch: disk={disk_plugins} vs catalog={cat_plugins}")
    if disk_themes != cat_themes:
        raise ValueError(f"Theme directory/catalog mismatch: disk={disk_themes} vs catalog={cat_themes}")
    if disk_locales != cat_locales:
        raise ValueError(f"Locale file/catalog mismatch: disk={disk_locales} vs catalog={cat_locales}")

    metas: list[dict] = []
    for pid in [str(x["id"]) for x in p_cat.get("plugins", [])]:
        metas.append(load_and_verify_metadata(repo_root, "plugin", pid))
    for tid in [str(x["id"]) for x in t_cat.get("themes", [])]:
        metas.append(load_and_verify_metadata(repo_root, "theme", tid))
    for lid in [str(x["id"]) for x in l_cat.get("items", [])]:
        metas.append(load_and_verify_metadata(repo_root, "locale", lid))

    tags = sorted({m["release_tag"] for m in metas})
    if len(tags) != 1:
        raise ValueError(
            f"All official extensions must share ONE unified release_tag (found multiple: {tags})"
        )
    return tags[0], metas


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


def resolve_git_epoch_for_path(repo_root: pathlib.Path, rel_path: str) -> str | None:
    forced = os.environ.get("SOURCE_DATE_EPOCH", "").strip()
    if forced:
        return forced
    try:
        proc = subprocess.run(
            ["git", "log", "-1", "--format=%ct", "--", rel_path],
            cwd=str(repo_root),
            capture_output=True,
            text=True,
            check=False,
        )
        out = proc.stdout.strip()
        if proc.returncode == 0 and out.isdigit():
            return out
    except OSError:
        pass
    return None


def verify_full_extension_tree(
    ext_dir: pathlib.Path, kind: str, ext_id: str, manifest: dict
) -> None:
    if kind == "locale":
        src_file = ext_dir / f"{ext_id}.json"
        if not src_file.is_file():
            raise FileNotFoundError(f"Missing locale source file: {src_file}")
        return

    if kind == "theme":
        tpl_dir = ext_dir / "templates"
        html_files = list(tpl_dir.rglob("*.html")) if tpl_dir.is_dir() else []
        if not html_files:
            raise FileNotFoundError(
                f"Incomplete theme tree for {ext_id!r}: no templates/*.html found"
            )
        return

    entry_rel = str(manifest.get("entry") or "main.js").strip()
    if not (ext_dir / entry_rel).is_file():
        raise FileNotFoundError(
            f"Incomplete checkout for {ext_id!r}: missing entry {entry_rel}"
        )
    if ext_id == "kanbanniang":
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
            if not (ext_dir / rel).is_file():
                raise FileNotFoundError(
                    f"Incomplete kanbanniang tree (sparse-checkout still active?): missing {rel}"
                )
        model_list = json.loads(
            (ext_dir / "assets" / "model-list.json").read_text(encoding="utf-8")
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


def resolve_node_binary() -> str:
    env_bin = os.environ.get("NODE_BIN", "").strip()
    if env_bin and pathlib.Path(env_bin).is_file():
        return env_bin
    found = shutil.which("node")
    if found:
        return found
    raise RuntimeError(
        "Node.js is required to minify extension JS/CSS/JSON assets (pass --no-minify to skip)"
    )


def minify_extension_files(
    repo_root: pathlib.Path,
    ext_dir: pathlib.Path,
    entry_rel: str,
    files: list[tuple[str, pathlib.Path]],
    tmp_dir: pathlib.Path,
) -> tuple[dict[str, pathlib.Path], int, int]:
    minifier_script = repo_root / ".github" / "scripts" / "minify-plugin-assets.mjs"
    admin_modules = repo_root / "web" / "admin" / "node_modules"
    if not minifier_script.is_file():
        raise FileNotFoundError(f"Missing minifier script: {minifier_script}")
    if not admin_modules.is_dir():
        raise FileNotFoundError(
            f"Missing {admin_modules} (run `npm ci --prefix web/admin` first or pass --no-minify)"
        )

    batch_items = []
    overrides: dict[str, pathlib.Path] = {}
    for idx, (arcname, src_path) in enumerate(files):
        if src_path.suffix.lower() not in MINIFIABLE_EXTENSIONS:
            continue
        rel = src_path.relative_to(ext_dir).as_posix()
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
            f"Extension asset minification failed (exit {proc.returncode}): {err_detail}"
        )

    summary = json.loads(proc.stdout.strip().splitlines()[-1])
    return overrides, int(summary.get("rawBytes", 0)), int(summary.get("minBytes", 0))


def build_extension_zip(
    repo_root: pathlib.Path,
    ext_dir: pathlib.Path,
    kind: str,
    ext_id: str,
    manifest: dict,
    out_zip: pathlib.Path,
    zip_dt: tuple[int, int, int, int, int, int],
    minify: bool = True,
) -> tuple[int, int, int, int]:
    files: list[tuple[str, pathlib.Path]] = []
    if kind == "locale":
        src_file = ext_dir / f"{ext_id}.json"
        if src_file.is_symlink():
            raise ValueError(f"Symlink forbidden in locale pack: {src_file}")
        files.append((f"{ext_id}.json", src_file))
    else:
        for p in ext_dir.rglob("*"):
            if p.is_symlink():
                raise ValueError(f"Symlink forbidden in {kind} directory: {p}")
            if not p.is_file():
                continue
            if p.name.lower() in EXCLUDED_NAMES or p.name.startswith("."):
                continue
            rel = p.relative_to(ext_dir).as_posix()
            files.append((f"{ext_id}/{rel}", p))

    files.sort(key=lambda item: item[0])
    out_zip.parent.mkdir(parents=True, exist_ok=True)
    if out_zip.exists():
        out_zip.unlink()

    entry_rel = (
        str(manifest.get("entry") or "main.js").strip()
        if kind == "plugin"
        else "__none__"
    )
    raw_total = 0
    text_raw_bytes = 0
    text_min_bytes = 0

    with tempfile.TemporaryDirectory(prefix="volo-ext-min-") as tmp_raw:
        tmp_dir = pathlib.Path(tmp_raw)
        overrides: dict[str, pathlib.Path] = {}
        if minify:
            if kind == "locale":
                compact_out = tmp_dir / f"{ext_id}.json"
                raw_text = files[0][1].read_text(encoding="utf-8")
                text_raw_bytes = len(raw_text.encode("utf-8"))
                compact_text = json.dumps(
                    json.loads(raw_text), ensure_ascii=False, separators=(",", ":")
                )
                compact_out.write_text(compact_text, encoding="utf-8")
                text_min_bytes = compact_out.stat().st_size
                overrides[files[0][0]] = compact_out
            else:
                overrides, text_raw_bytes, text_min_bytes = minify_extension_files(
                    repo_root, ext_dir, entry_rel, files, tmp_dir
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


def write_single_release_notes(meta: dict, out_notes: pathlib.Path, repo: str) -> None:
    kind = meta["kind"]
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
        f"- **ID**: `{vid}` (`{kind}`)",
        f"- **Version**: `v{ver}` (`{tag}`)",
        f"- **Min Host Version**: `v{min_host}`",
        f"- **Release Assets**: `{asset}` (alias: `{alias}`)",
        "",
        f"- {zh_sum}",
        f"- {en_sum}",
    ]
    if repo:
        lines.append(f"- **Repository**: https://github.com/{repo}")
    out_notes.write_text("\n".join(lines) + "\n", encoding="utf-8")


def write_bundle_release_notes(
    bundle_tag: str, metas: list[dict], out_notes: pathlib.Path, repo: str
) -> None:
    bundle_ver = bundle_tag.removeprefix("extensions-")
    kind_labels = {
        "plugin": "插件 / Plugin",
        "theme": "主题 / Theme",
        "locale": "语言包 / Locale",
    }
    lines = [
        f"## Volo-go 官方扩展合集包 ({bundle_ver}) / Official Extensions Bundle ({bundle_ver})",
        "",
        "本 Release 统一归档分发所有官方**独立插件包、外部主题包与多语言扩展包**（`.zip` + 稳定无版本别名 + 统一 `SHA256SUMS.txt` 校验清单），供单二进制部署按需直装及管理后台一键在线热更新使用。",
        "",
        "This unified standalone release consolidates all official **Plugin, Theme, and Locale `.zip` archives** (versioned `.zip`, stable alias `.zip`, and unified `SHA256SUMS.txt`) for URL direct installation and 1-click transactional online updates.",
        "",
        "| 类型 / Kind | ID | 版本 / Version | 最低宿主 / Min Host | 版本化归档 / Versioned Asset | 稳定别名 / Alias |",
        "| :--- | :--- | :--- | :--- | :--- | :--- |",
    ]
    for m in metas:
        klabel = kind_labels.get(m["kind"], m["kind"])
        lines.append(
            f"| {klabel} | `{m['id']}` | `v{m['version']}` | `v{m['min_host_version']}` | `{m['release_asset']}` | `{m['release_asset_alias']}` |"
        )
    lines.extend(
        [
            "",
            "### 使用说明 / Usage",
            "- **后台一键安装 / 在线更新**：登录 `Volo-go` 管理工作台 -> **「外观与扩展」** -> 支持官方目录一键直装、输入直链 URL 安装、上传本地 `.zip` 或点击「检测更新」一键原子热升级（自动校验 `SHA256SUMS.txt`、保留已有配置并在异常时自动回滚）。",
            "- **Admin Console Direct Install & Online Update**: Open `Volo-go` Admin Console -> **Appearance & Extensions** -> install directly from Official Catalog / URL, upload `.zip`, or run **Check Updates** for SHA-256 verified 1-click online upgrades with automatic rollback protection.",
        ]
    )
    if repo:
        lines.extend(
            [
                "",
                f"- **Catalogs**: [`plugins/catalog.json`](https://github.com/{repo}/blob/main/plugins/catalog.json) · [`themes/catalog.json`](https://github.com/{repo}/blob/main/themes/catalog.json) · [`locales/catalog.json`](https://github.com/{repo}/blob/main/locales/catalog.json)",
            ]
        )
    out_notes.write_text("\n".join(lines) + "\n", encoding="utf-8")


def package_single_meta(
    repo_root: pathlib.Path,
    meta: dict,
    out_dir: pathlib.Path,
    minify: bool,
    merged_sums: dict[str, str],
) -> None:
    epoch = resolve_git_epoch_for_path(repo_root, meta["rel_path"])
    zip_dt = resolve_zip_date_time(epoch)

    versioned_zip = out_dir / meta["release_asset"]
    alias_zip = out_dir / meta["release_asset_alias"]

    verify_full_extension_tree(meta["ext_dir"], meta["kind"], meta["id"], meta["manifest"])
    file_count, raw_bytes, text_raw, text_min = build_extension_zip(
        repo_root,
        meta["ext_dir"],
        meta["kind"],
        meta["id"],
        meta["manifest"],
        versioned_zip,
        zip_dt,
        minify=minify,
    )
    shutil.copyfile(versioned_zip, alias_zip)

    digest = sha256_file(versioned_zip)
    short_name = f"{meta['id']}-{meta['version']}.zip"
    merged_sums[versioned_zip.name] = digest
    merged_sums[alias_zip.name] = digest
    if short_name not in (versioned_zip.name, alias_zip.name):
        merged_sums[short_name] = digest

    (out_dir / f"{versioned_zip.name}.sha256").write_text(
        f"{digest}  {versioned_zip.name}\n",
        encoding="utf-8",
    )

    zip_bytes = versioned_zip.stat().st_size
    min_info = (
        f", text_minified={text_raw}B->{text_min}B"
        if minify and text_raw > 0
        else ""
    )
    print(
        f"Packaged {meta['kind']}:{meta['id']} v{meta['version']}: {file_count} files, "
        f"unpacked={raw_bytes / (1024 * 1024):.2f} MiB, zip={zip_bytes / (1024 * 1024):.2f} MiB ({zip_bytes}B)"
        f"{min_info}, sha256={digest}"
    )


def load_existing_sums(sums_txt: pathlib.Path) -> dict[str, str]:
    merged: dict[str, str] = {}
    if sums_txt.is_file():
        for raw_line in sums_txt.read_text(encoding="utf-8").splitlines():
            parts = raw_line.strip().split()
            if len(parts) >= 2:
                merged[parts[-1].lstrip("*")] = parts[0]
    return merged


def write_sums_file(sums_txt: pathlib.Path, merged_sums: dict[str, str]) -> None:
    sums_lines = [f"{merged_sums[name]}  {name}" for name in sorted(merged_sums)]
    sums_txt.write_text("\n".join(sums_lines) + "\n", encoding="utf-8")


def main() -> int:
    parser = argparse.ArgumentParser(
        description="Package Volo-go official standalone extensions (single item or unified bundle)"
    )
    target_group = parser.add_mutually_exclusive_group(required=True)
    target_group.add_argument("--plugin", help="Plugin ID (e.g. kanbanniang, sensitive-words)")
    target_group.add_argument("--theme", help="Theme ID (e.g. volo-journal)")
    target_group.add_argument("--locale", help="Locale code (e.g. zh-TW, ja-JP)")
    target_group.add_argument(
        "--bundle",
        action="store_true",
        help="Package ALL official plugins, themes & locales into one unified release directory",
    )
    target_group.add_argument(
        "--verify-all-meta",
        action="store_true",
        help="Verify 1-to-1 directory/catalog parity and unified release metadata across all official extensions",
    )
    target_group.add_argument(
        "--print-bundle-meta",
        action="store_true",
        help="Verify all extensions and print unified release_tag, bundle_version, and expected_assets",
    )
    parser.add_argument(
        "--repo-root",
        default=".",
        help="Repository root directory",
    )
    parser.add_argument(
        "--print-meta",
        action="store_true",
        help="Verify single-extension manifest/catalog parity and print KEY=VALUE lines without building ZIP",
    )
    parser.add_argument(
        "--output-dir",
        default="dist/plugin-release",
        help="Output directory for built ZIPs, alias ZIPs, SHA256SUMS.txt, and release notes",
    )
    parser.add_argument(
        "--repo",
        default="",
        help="GitHub repository slug (owner/name) for release notes",
    )
    parser.add_argument(
        "--no-minify",
        action="store_true",
        help="Skip AST minification of .js, .css, and .json assets",
    )
    args = parser.parse_args()

    repo_root = pathlib.Path(args.repo_root).resolve()

    if args.verify_all_meta or args.print_bundle_meta or args.bundle:
        bundle_tag, metas = verify_all_extensions(repo_root)
        bundle_ver = bundle_tag.removeprefix("extensions-")
        expected_assets: list[str] = []
        for m in metas:
            expected_assets.append(m["release_asset"])
            expected_assets.append(f"{m['release_asset']}.sha256")
            expected_assets.append(m["release_asset_alias"])
        expected_assets.append("SHA256SUMS.txt")

        if args.verify_all_meta:
            print(
                f"Verified {len(metas)} official extensions -> unified tag {bundle_tag} "
                f"({', '.join(m['kind'] + ':' + m['id'] + '@' + m['version'] for m in metas)})"
            )
            return 0

        if args.print_bundle_meta:
            print(f"release_tag={bundle_tag}")
            print(f"bundle_version={bundle_ver}")
            print(f"extension_count={len(metas)}")
            print(f"expected_assets={','.join(expected_assets)}")
            return 0

        out_dir = pathlib.Path(args.output_dir).resolve()
        if out_dir.exists():
            shutil.rmtree(out_dir)
        out_dir.mkdir(parents=True, exist_ok=True)
        sums_txt = out_dir / "SHA256SUMS.txt"
        merged_sums: dict[str, str] = {}
        for m in metas:
            package_single_meta(
                repo_root, m, out_dir, minify=not args.no_minify, merged_sums=merged_sums
            )
        write_sums_file(sums_txt, merged_sums)
        write_bundle_release_notes(
            bundle_tag, metas, out_dir / "extensions-release-notes.md", args.repo
        )
        return 0

    if args.theme:
        kind = "theme"
        ext_id = args.theme.strip()
    elif args.locale:
        kind = "locale"
        ext_id = args.locale.strip()
    else:
        kind = "plugin"
        ext_id = args.plugin.strip()

    meta = load_and_verify_metadata(repo_root, kind, ext_id)
    if args.print_meta:
        print(f"kind={meta['kind']}")
        print(f"extension_id={meta['id']}")
        if kind == "plugin":
            print(f"plugin_id={meta['id']}")
        elif kind == "theme":
            print(f"theme_id={meta['id']}")
        else:
            print(f"locale_id={meta['id']}")
        print(f"version={meta['version']}")
        print(f"release_tag={meta['release_tag']}")
        print(f"release_asset={meta['release_asset']}")
        print(f"release_asset_alias={meta['release_asset_alias']}")
        print(f"min_host_version={meta['min_host_version']}")
        return 0

    out_dir = pathlib.Path(args.output_dir).resolve()
    out_dir.mkdir(parents=True, exist_ok=True)
    sums_txt = out_dir / "SHA256SUMS.txt"
    merged_sums = load_existing_sums(sums_txt)
    package_single_meta(
        repo_root, meta, out_dir, minify=not args.no_minify, merged_sums=merged_sums
    )
    write_sums_file(sums_txt, merged_sums)
    write_single_release_notes(meta, out_dir / "plugin-release-notes.md", args.repo)
    return 0


if __name__ == "__main__":
    sys.exit(main())
