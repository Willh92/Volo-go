# Volo-go Official Plugin Catalog

> 中文文档：[README.zh-CN.md](README.zh-CN.md) · Machine-readable registry: [`catalog.json`](catalog.json)

This directory hosts the official sandboxed plugins for `Volo-go`, their canonical latest version numbers, and their distribution metadata. Every plugin runs inside the pure-Go ES6 sandboxed VM (`runtime: "goja"`) with declarative capability permissions, isolated `plugin_kv` storage, and hot-swappable `.zip` lifecycle support.

---

## 1. Latest Official Plugin Versions

| Plugin ID | Name | Latest Version | Min Host Version | Distribution Mode | Bundled in Core Archive / Docker | Summary |
| :--- | :--- | :---: | :---: | :--- | :---: | :--- |
| [`sensitive-words`](sensitive-words/) | Sensitive Word & Nickname Guard | **`v1.1.0`** | `0.1.0` | `bundled` | Yes | 64k+ categorized word guard powered by the host 16B/state BFS-CSR Aho-Corasick engine for comments and nicknames, with online dictionary sync. |
| [`kanbanniang`](kanbanniang/) | Live2D Mascot Companion | **`v2.0.0`** | `0.1.0` | `standalone-release` | No (Install on demand) | Full self-hosted pack of 26 offline Live2D Cubism 2.1 models, 150 Pio/Tia outfit variants, 7-action hover/touch toolbar, pointer drag, animated WebP sakura backgrounds, and KV head-pats with pure-WebP slimmed textures (~45 MiB) + custom PNG pack compatibility. |

---

## 2. Split Packaging & Version-Deduplicated Release Automation

### 2.1 Why Is `kanbanniang` Distributed as a Standalone `.zip`?

- **Keep Core Archives & Container Images Slim**: Each `Volo-go` OS/arch binary bundle is roughly **`~8 MB`**, and the Distroless container image stays minimal. Packaging [`kanbanniang`](kanbanniang/) (26 full Live2D models, motions, voices, `287` Alpha `.webp` textures + animated `.webp` sakura backgrounds, ~45 MiB total) as an independent release artifact avoids re-downloading static model assets on every core engine upgrade.
- **In-Tree Source & Full CI Verification**: `plugins/kanbanniang/` remains tracked in Git so every commit verifies all 26 model descriptors, strict local-asset confinement, and catalog version parity.

### 2.2 Automated Version-Gated GitHub Actions Packaging

Managed automatically by [`.github/workflows/plugin-kanbanniang.yml`](../.github/workflows/plugin-kanbanniang.yml), [`.github/workflows/release.yml`](../.github/workflows/release.yml), and [`.github/scripts/package-plugin.py`](../.github/scripts/package-plugin.py):

1. **Single Version Truth**: The packager reads `version` from [`kanbanniang/manifest.json`](kanbanniang/manifest.json) (currently **`2.0.0`**) and verifies exact parity against [`catalog.json`](catalog.json).
2. **Never Re-Packaged or Re-Uploaded When Version Is Unchanged**:
   - Standalone plugin releases use the deterministic tag `plugin-kanbanniang-v<version>` (currently **`plugin-kanbanniang-v2.0.0`**) shipping `plugin-kanbanniang-v2.0.0.zip`, stable alias `plugin-kanbanniang.zip`, and `SHA256SUMS.txt`.
   - On both `main` pushes and `Volo-go vX.Y.Z` core releases, GitHub Actions first checks whether GitHub Release `plugin-kanbanniang-v<version>` already exists. **When the plugin version is unchanged and already published, CI skips compression and upload immediately**, avoiding redundant builds and duplicate release storage. Plugin releases always set `make_latest: false` so `releases/latest` stays pinned to the core `Volo-go` binary release.
3. **Publishing a New Plugin Version**: Bump `version` in both [`kanbanniang/manifest.json`](kanbanniang/manifest.json) and [`catalog.json`](catalog.json) (e.g., to `2.0.1`) and push to `main`; GitHub Actions automatically builds a reproducible archive (`ZIP_STORED` for pre-compressed media, `ZIP_DEFLATED` for `.moc`/`.mtn`/`.js`/`.css`/`.json`) and publishes the new `plugin-kanbanniang-v<version>` release.

---

## 3. Installation & In-Place Upgrade

1. **One-Click Admin Console Upload (Recommended)**:
   - Download `plugin-kanbanniang-v2.0.0.zip` (or `plugin-kanbanniang.zip`) from GitHub Releases.
   - Open Admin Console -> **Appearance & Extensions -> Plugins** -> upload the `.zip` archive (with overwrite enabled when upgrading an existing installation to preserve settings and head-pat counters), then enable the plugin.
2. **Filesystem / Docker Volume Unpack**:
   - Extract `plugin-kanbanniang-v2.0.0.zip` into `VOLO_PLUGINS_DIR` (default `./plugins`, producing `./plugins/kanbanniang/manifest.json`) and reload plugins in the Admin Console or restart `volo`.
3. **From Source Checkout**:
   - Both `./plugins/kanbanniang` and `./plugins/sensitive-words` are already present in a Git checkout with no extra download required.
