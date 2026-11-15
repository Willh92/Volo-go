# Volo-go Official Plugin Catalog

> 中文文档：[README.zh-CN.md](README.zh-CN.md) · Machine-readable registry: [`catalog.json`](catalog.json)

This directory hosts the official sandboxed plugins for `Volo-go`, their canonical latest version numbers, and their distribution metadata. Every plugin runs inside the pure-Go ES6 sandboxed VM (`runtime: "goja"`) with declarative capability permissions, isolated `plugin_kv` storage, and hot-swappable `.zip` lifecycle support.

---

## 1. Latest Official Plugin Versions

| Plugin ID | Name | Latest Version | Min Host Version | Distribution Mode | Bundled in Core Archive / Docker | Summary |
| :--- | :--- | :---: | :---: | :--- | :---: | :--- |
| [`sensitive-words`](sensitive-words/) | Sensitive Word & Nickname Guard | **`v1.1.0`** | `0.1.0` | `bundled` + `plugin-sensitive-words-v1.1.0.zip` | Yes | 64k+ categorized word guard powered by the host 16B/state BFS-CSR Aho-Corasick engine for comments and nicknames, with online dictionary sync and 1-click online hot-update. |
| [`kanbanniang`](kanbanniang/) | Live2D Mascot Companion | **`v2.0.5`** | `0.1.0` | `standalone-release` (`plugin-kanbanniang-v2.0.5.zip`) | No (Install on demand) | Full self-hosted pack of 26 offline Live2D Cubism 2.1 models, 150 Pio/Tia outfit variants, parallel `.moc`/`.webp`/motion prefetch, 7-action hover/touch/keyboard toolbar, non-destructive corner minimize/summon pill, pointer drag, animated WebP sakura backgrounds, manual retry error card, and KV head-pats (`~35.5 MiB` archive) + custom PNG pack compatibility. |

> Official external theme standalone archive: [`themes/volo-journal`](../themes/volo-journal/) (tracked in [`themes/catalog.json`](../themes/catalog.json), currently **`v1.1.1`**, bundled in core releases and also published as standalone `theme-volo-journal-v1.1.1.zip` for independent hot-updates).

---

## 2. Split Packaging & Version-Deduplicated Release Automation

### 2.1 Standalone `.zip` Packages for All Official Extensions & Slim Core Releases

- **Independent `.zip` Archives + `SHA256SUMS.txt` for Every Official Extension**: `sensitive-words` (`plugin-sensitive-words-v<ver>.zip`), `kanbanniang` (`plugin-kanbanniang-v<ver>.zip`), and the official magazine theme `volo-journal` (`theme-volo-journal-v<ver>.zip`) are all packaged deterministically via [`.github/scripts/package-plugin.py`](../.github/scripts/package-plugin.py), allowing bare-binary deployments to install on demand or hot-update extensions independently of core host upgrades.
- **Keep Core Archives & Container Images Slim**: `sensitive-words` and `volo-journal` remain bundled out-of-the-box in core OS/arch archives (`~8 MB`) and Docker images, while [`kanbanniang`](kanbanniang/) (`~35.5 MiB` of 26 offline Live2D models and `287` Alpha `.webp` textures) stays standalone-only so core upgrades never re-download heavy model assets.
- **In-Tree Source & Full CI Verification**: Every official plugin and theme remains tracked in Git and verified in CI for manifest/catalog parity and deterministic archive generation.

### 2.2 Automated Version-Gated GitHub Actions Packaging

Managed automatically by [`.github/workflows/plugin-kanbanniang.yml`](../.github/workflows/plugin-kanbanniang.yml), [`.github/workflows/release.yml`](../.github/workflows/release.yml), and [`.github/scripts/package-plugin.py`](../.github/scripts/package-plugin.py):

1. **Single Version Truth**: The packager reads `version` from each extension's `manifest.json` / `theme.json` and enforces exact parity against [`plugins/catalog.json`](catalog.json) and [`themes/catalog.json`](../themes/catalog.json).
2. **Consolidated Extensions Bundle Release & Deduplicated Uploads**:
   - All official standalone plugin, theme, and locale `.zip` packages share ONE unified GitHub Release tag `extensions-v<version>` (e.g., `extensions-v1.0.3`), publishing versioned `.zip` archives (`plugin-sensitive-words-v1.1.0.zip`, `plugin-kanbanniang-v2.0.5.zip`, `theme-volo-journal-v1.1.1.zip`, `locale-zh-TW-v1.0.0.zip`, `locale-ja-JP-v1.0.0.zip`), stable unversioned aliases (e.g., `plugin-kanbanniang.zip`, `theme-volo-journal.zip`), and a unified `SHA256SUMS.txt` — avoiding separate per-plugin / per-theme release clutter.
   - On both `main` pushes and `Volo-go vX.Y.Z` core releases, GitHub Actions checks whether all expected archives already exist on the unified `extensions-v<version>` release: **when unchanged and already published, CI skips compression and upload immediately**, and always sets `make_latest: false` so `releases/latest` stays pinned to the core `Volo-go` binary release.
3. **Publishing a New Extension Version**: Bump `version` in the extension's `manifest.json` (or `theme.json`) and update `release_tag` (`extensions-v<version>`) + `version` in `catalog.json`, then push to `main` to automatically minify, package, and publish the consolidated bundle release.

---

## 3. Installation, Direct URL Install & Atomic Online Updates

1. **1-Click URL / Official Catalog Install in Admin Console (Recommended)**:
   - Open Admin Console -> **Appearance & Extensions -> Plugins (or Themes)** -> click **Install from URL**.
   - Click **Install** directly on any official standalone package in the built-in catalog card list, or paste any public HTTPS `.zip` URL (with optional 64-char `SHA-256` pinning). Downloads stream straight to disk via `OutboundSafeDialer` (`< 64 KiB` heap overhead) and can be aborted at any instant without touching live state.
2. **On-Demand Update Checks & Two-Phase Transactional Online Upgrade**:
   - Click **Check Updates** (zero background polling timers; `0` idle network/RAM cost) to inspect changelogs and run **1-Click Online Update** with automatic self-healing rollback if network transfer, checksum verification, or post-swap boot fails.
3. **Local `.zip` Upload or Filesystem Unpack**:
   - Upload a `.zip` file via **Install (.zip)** in the Admin Console, or extract into `VOLO_PLUGINS_DIR` (default `./plugins`) and reload.
4. **From Source Checkout**:
   - `./plugins/kanbanniang`, `./plugins/sensitive-words`, and `./themes/volo-journal` are already present in a Git checkout with no extra download required.
