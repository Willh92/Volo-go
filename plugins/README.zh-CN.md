# Volo-go 官方插件目录 (Official Plugin Catalog)

> English version: [README.md](README.md) · 机器可读清单：[`catalog.json`](catalog.json)

本目录维护 `Volo-go` 官方沙箱插件的源码、最新版本号与分发策略。所有插件均运行于宿主纯 Go ES6 沙箱虚拟机（`runtime: "goja"`），遵循统一声明式权限、`plugin_kv` 物理隔离与热插拔 `.zip` 协议。

---

## 1. 官方插件最新版本一览

| 插件 ID | 名称 | 最新版本 | 最低宿主版本 | 分发方式 | 核心主包 / Docker 默认内置 | 说明 |
| :--- | :--- | :---: | :---: | :--- | :---: | :--- |
| [`sensitive-words`](sensitive-words/) | 敏感词与昵称卫士 | **`v1.1.0`** | `0.1.0` | 随核心包内置 (`bundled`) | 是 | 内置 6.4 万级分类词库，基于宿主 16B/状态 BFS-CSR Aho-Corasick 自动机守护评论与昵称，支持在线词库同步。 |
| [`kanbanniang`](kanbanniang/) | Live2D 看板娘 | **`v2.0.2`** | `0.1.0` | 独立插件包发布 (`standalone-release`) | 否（按需安装） | 完整内置 26 套本地离线 Live2D Cubism 2.1 模型、Pio/Tia 150 套随机换装、`.moc`/`.webp`/动作并行预取、7 键悬浮/触控工具栏、拖拽记忆、樱花动态背景与摸头计数，采用纯 WebP 轻量化纹理（归档约 `35.5 MiB`）并兼容自定义 PNG 模型包。 |

---

## 2. 分包设计与自动去重发布机制

### 2.1 为什么 `kanbanniang` 采用独立 `.zip` 分发？

- **保持核心发行包与容器镜像极致轻量**：`Volo-go` 各平台编译产物归档仅约 **`~8 MB`**，Distroless 容器镜像保持精简；而 [`kanbanniang`](kanbanniang/) 完整离线收录了 26 套高精度 Live2D 模型、动作、语音及纯 WebP 轻量化纹理（`287` 张 Alpha `.webp` 贴图 + 动态 `.webp` 樱花背景，归档约 `35.5 MiB` / 解压约 `44.9 MiB`），独立分包可避免每次升级主程序时重复拉取静态模型资源。
- **源码同仓维护，端到端回归验证**：仓库内完整保留 `plugins/kanbanniang/`，CI 每次提交均自动校验全部 26 套模型的本地路径安全性、版本一致性与沙箱接口。

### 2.2 GitHub Actions 版本感知自动去重打包

由 [`.github/workflows/plugin-kanbanniang.yml`](../.github/workflows/plugin-kanbanniang.yml)、[`.github/workflows/release.yml`](../.github/workflows/release.yml) 与 [`.github/scripts/package-plugin.py`](../.github/scripts/package-plugin.py) 协同自动化管理：

1. **单一事实版本源**：打包脚本读取 [`kanbanniang/manifest.json`](kanbanniang/manifest.json) 中的 `version`（当前为 **`2.0.2`**），并强制校验与 [`catalog.json`](catalog.json) 及 `main.js` 中登记的版本号完全一致。
2. **版本不变绝不重复打包或重复上传**：
   - 对应的 GitHub Release 标签固定为 `plugin-kanbanniang-v<version>`（当前：**`plugin-kanbanniang-v2.0.2`**），附件提供带版本号归档 `plugin-kanbanniang-v2.0.2.zip`、稳定别名 `plugin-kanbanniang.zip` 与 `SHA256SUMS.txt`。
   - 无论是 `main` 分支提交还是发布新的 `Volo-go vX.Y.Z` 主程序版本，工作流都会先检查 `plugin-kanbanniang-v<version>` Release 是否已存在：**若版本号未变更且归档已发布，CI 直接秒级跳过压缩与上传**，零重复构建、零冗余存储，且插件 Release 始终标记 `make_latest: false`，绝不抢占主程序 `releases/latest` 指针。
3. **升级发布流程**：当后续更新看板娘脚本或模型资源需发布新包时，同步递增 [`kanbanniang/manifest.json`](kanbanniang/manifest.json)、[`kanbanniang/main.js`](kanbanniang/main.js) 与 [`catalog.json`](catalog.json) 的 `version`（如 `2.0.3`），推送至 `main` 分支即自动执行 AST 压缩（`.js`/`.css`/`.json`）与确定性分包（已压缩纹理采用 `ZIP_STORED` 零二次膨胀，`.moc`/`.mtn`/`.js` 采用 `ZIP_DEFLATED`）并发布新版本 Release。

---

## 3. 安装与原位升级方式

1. **管理台一键上传安装 / 覆盖升级（推荐）**：
   - 前往 GitHub Releases 下载对应版本的 `plugin-kanbanniang-v2.0.2.zip`（或 `plugin-kanbanniang.zip`）。
   - 登录管理工作台 -> **「外观与扩展 -> 插件扩展」** -> 点击 **「上传安装 (.zip)」**（已安装旧版时勾选原位覆盖升级即可无缝保留已有配置与摸头统计数据），启用插件即可。
2. **宿主机 / 容器目录直装**：
   - 将 `plugin-kanbanniang-v2.0.2.zip` 解压至 `VOLO_PLUGINS_DIR`（默认 `./plugins`，解压后路径为 `./plugins/kanbanniang/manifest.json`），在管理台点击刷新或重启服务即可发现并启用。
3. **源码开发环境**：
   - 克隆本仓库本地运行时，`./plugins/kanbanniang` 与 `./plugins/sensitive-words` 均已在目录中就绪，无需额外下载。
