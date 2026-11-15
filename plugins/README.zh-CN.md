# Volo-go 官方插件目录 (Official Plugin Catalog)

> English version: [README.md](README.md) · 机器可读清单：[`catalog.json`](catalog.json)

本目录维护 `Volo-go` 官方沙箱插件的源码、最新版本号与分发策略。所有插件均运行于宿主纯 Go ES6 沙箱虚拟机（`runtime: "goja"`），遵循统一声明式权限、`plugin_kv` 物理隔离与热插拔 `.zip` 协议。

---

## 1. 官方插件最新版本一览

| 插件 ID | 名称 | 最新版本 | 最低宿主版本 | 分发方式 | 核心主包 / Docker 默认内置 | 说明 |
| :--- | :--- | :---: | :---: | :--- | :---: | :--- |
| [`sensitive-words`](sensitive-words/) | 敏感词与昵称卫士 | **`v1.1.0`** | `0.1.0` | 核心内置 + 独立包发布 (`bundled` + `plugin-sensitive-words-v1.1.0.zip`) | 是 | 内置 6.4 万级分类词库，基于宿主 16B/状态 BFS-CSR Aho-Corasick 自动机守护评论与昵称，支持在线词库同步与一键在线热更新。 |
| [`kanbanniang`](kanbanniang/) | Live2D 看板娘 | **`v2.0.5`** | `0.1.0` | 独立插件包发布 (`standalone-release` · `plugin-kanbanniang-v2.0.5.zip`) | 否（按需安装） | 完整内置 26 套本地离线 Live2D Cubism 2.1 模型、Pio/Tia 150 套随机换装、`.moc`/`.webp`/动作并行预取、7 键悬浮/触控/键盘工具栏、贴边收起与随时唤醒胶囊、拖拽记忆、樱花动态背景、加载失败就地重试卡片与摸头计数，采用纯 WebP 轻量化纹理（归档约 `35.5 MiB`）并兼容自定义 PNG 模型包。 |

> 同步提供官方外部纸刊主题独立归档包：[`themes/volo-journal`](../themes/volo-journal/)（机器可读清单 [`themes/catalog.json`](../themes/catalog.json)，当前 **`v1.1.1`**，同时内置于核心包并提供独立 `theme-volo-journal-v1.1.1.zip` 与一键在线热更新）。

---

## 2. 分包设计与自动去重发布机制

### 2.1 全量官方扩展独立 `.zip` 包与核心包体积控制

- **所有官方扩展均提供独立 `.zip` 包与 `SHA256SUMS.txt`**：`sensitive-words`（`plugin-sensitive-words-v<ver>.zip`）、`kanbanniang`（`plugin-kanbanniang-v<ver>.zip`）与官方主题 `volo-journal`（`theme-volo-journal-v<ver>.zip`）均通过 [`.github/scripts/package-plugin.py`](../.github/scripts/package-plugin.py) 生成确定性独立 `.zip` 包，方便单二进制裸机部署按需直装或无需升级宿主主程序直接热更新扩展。
- **保持核心发行包与容器镜像极致轻量**：`sensitive-words` 与 `volo-journal` 继续随核心包与 Docker 镜像开箱内置（核心归档保持 **`~8 MB`**）；而体积较大的 [`kanbanniang`](kanbanniang/)（26 套完整离线模型与 `287` 张 Alpha `.webp` 贴图，归档约 `35.5 MiB`）保持按需独立安装，避免每次升级主程序重复下载静态模型资源。
- **源码同仓维护，端到端回归验证**：仓库内完整保留所有官方插件与主题源码，CI 每次提交均强制校验 `manifest.json` / `theme.json` 与 `catalog.json` 的版本一致性及确定性打包流程。

### 2.2 GitHub Actions 版本感知自动去重打包

由 [`.github/workflows/plugin-kanbanniang.yml`](../.github/workflows/plugin-kanbanniang.yml)、[`.github/workflows/release.yml`](../.github/workflows/release.yml) 与 [`.github/scripts/package-plugin.py`](../.github/scripts/package-plugin.py) 协同自动化管理：

1. **单一事实版本源**：打包脚本读取各扩展 `manifest.json` / `theme.json` 中的 `version`，并强制校验与 [`plugins/catalog.json`](catalog.json) / [`themes/catalog.json`](../themes/catalog.json) 完全一致。
2. **统一合并发布扩展合集包且自动去重**：
   - 所有官方独立插件、主题与语言包的 `.zip` 归档统一合并发布在同一个扩展合集 GitHub Release 标签 `extensions-v<version>` 下（如 `extensions-v1.0.3`），同时附带带版本号归档（`plugin-sensitive-words-v1.1.0.zip`、`plugin-kanbanniang-v2.0.5.zip`、`theme-volo-journal-v1.1.1.zip`、`locale-zh-TW-v1.0.0.zip`、`locale-ja-JP-v1.0.0.zip`）、稳定无版本别名（如 `plugin-kanbanniang.zip`、`theme-volo-journal.zip`）与统一 `SHA256SUMS.txt`，避免产生零散的单插件/单主题 Release。
   - 无论是 `main` 分支提交还是发布新的 `Volo-go vX.Y.Z` 主程序版本，工作流都会校验合集 Release 下的全部预期资产是否均已就绪：**若未发生变更且归档均已发布，CI 直接秒级跳过压缩与上传**，且扩展合集 Release 始终标记 `make_latest: false`，绝不抢占主程序 `releases/latest` 指针。
3. **升级发布流程**：当更新任一官方插件或主题时，同步递增其 `manifest.json`（或 `theme.json`）的 `version` 及各 `catalog.json` 的统一 `release_tag`（`extensions-v<version>`），推送至 `main` 分支即自动执行 AST 压缩并合并发布新版扩展合集 Release。

---

## 3. 安装、URL 直装与在线原子热更新

1. **管理台「URL / 官方源安装」一键直装（推荐）**：
   - 登录管理工作台 -> **「外观与扩展 -> 插件扩展（或主题皮肤）」** -> 点击 **「URL / 官方源安装」**。
   - 可直接在官方独立扩展包列表中点击 **「一键安装」**，或粘贴任意公开 HTTPS `.zip` 下载地址（可选填入 64 位 `SHA-256` 校验值）；服务端采用 `OutboundSafeDialer` 防 SSRF 流式直写临时磁盘（内存开销 `< 64 KiB`），支持随时点击 **「中断下载」** 取消且不影响线上运行状态。
2. **按需版本检测与双阶段原子在线更新**：
   - 点击 **「检测更新」**（零后台轮询，平时 `0` 网络与内存开销），若发现新版本可查看更新日志并一键执行 **「在线更新」**；下载中断、校验不通过或新版本启动异常均会自动无损回退至旧版本并完整保留已有配置与 `plugin_kv` 数据。
3. **本地 `.zip` 上传或宿主机目录直装**：
   - 也可在后台点击 **「上传安装 (.zip)」**，或将独立 `.zip` 解压至 `VOLO_PLUGINS_DIR`（默认 `./plugins`）后点击发现重载。
4. **源码开发环境**：
   - 克隆本仓库本地运行时，`./plugins/kanbanniang`、`./plugins/sensitive-words` 与 `./themes/volo-journal` 均已在目录中就绪，无需额外下载。
