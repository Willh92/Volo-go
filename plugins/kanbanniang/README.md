# kanbanniang (Volo-go Live2D Mascot Plugin)

## 概述

`kanbanniang` 为 `Volo-go` 提供纯本地化的 WebGL Live2D 互动看板娘挂件，内置 26 套经典模型、随机换装贴图、樱花动态背景、`tips.json` 交互语录、拖拽位置记忆、PJAX 跨页常驻以及 `plugin_kv` 摸头计数。所有模型、贴图与脚本均由本站 `/plugins/kanbanniang/assets/*` 提供，零第三方运行时外链。

## 资源与自定义兼容性说明

- **纯 WebP 轻量化纹理与动态背景（~45 MiB）**：`assets/model/<Group>/<Name>/index.json`（及 `textures.json`）与 `assets/background/sakura*.webp` 直接采用高压缩比 Alpha WebP 2 的幂（Power-of-Two）贴图与 Animated WebP 动态背景，在完整保留 26 套 Live2D Cubism 2.1 模型、150 套 Pio/Tia 换装、动作与语音的同时剔除冗余 `.png`/`.gif` 副本（体积缩减约 `85.9 MiB`）。
- **自定义 `.png` 模型包无缝兼容**：插件服务端与前端保留对第三方自定义 `.png` 模型包的原生兼容支持——若自行放入仅含 `.png` 贴图的自定义模型目录并在 `model-list.json` 注册，插件会自动校验本地路径并直出 `.png`（若同时包含同名 `.webp` 贴图则按需渐进启用）。

## 版权与使用须知

- 插件控制与适配代码遵循同目录 `LICENSE`（MIT 协议），完整上游来源见根目录 `THIRD_PARTY_NOTICES.md` 与 `SOURCES.json`。
- 内置 Live2D 模型与贴图的著作权归各原作者所有，仅供个人学习与研究展示，不得用于商业用途。
