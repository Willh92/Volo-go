# Third-Party Open Source Notices / 第三方开源组件许可证声明

`Volo-go` is licensed under the **Apache License, Version 2.0** ([LICENSE](./LICENSE)).
`Volo-go` 以 **Apache License 2.0** 协议开源发布（详见 [LICENSE](./LICENSE)）。

Binary distributions and container images of `Volo-go` incorporate the following pure-Go (`CGO_ENABLED=0`) and frontend open-source components, all of which are compatible with Apache-2.0 redistribution:
`Volo-go` 的单文件静态二进制与容器镜像包含以下全部兼容 Apache-2.0 分发的纯 Go（`CGO_ENABLED=0`）与前端开源依赖组件：

---

## 1. Direct Go Runtime Dependencies (`go.mod`) / Go 后端直接依赖

| Module / 模块 | Version / 版本 | License / 许可证 | Purpose / 用途 | Upstream / 来源 |
| :--- | :--- | :--- | :--- | :--- |
| `modernc.org/sqlite` | `v1.61.1` | BSD-3-Clause | Pure-Go (`CGO_ENABLED=0`) SQLite WAL engine | https://gitlab.com/cznic/sqlite |
| `github.com/go-sql-driver/mysql` | `v1.10.1` | MPL-2.0 | Pure-Go MySQL 8+ / MariaDB / TiDB driver (unmodified module) | https://github.com/go-sql-driver/mysql |
| `github.com/yuin/goldmark` | `v1.8.7` | MIT | CommonMark + GFM Markdown AST parser & renderer | https://github.com/yuin/goldmark |
| `github.com/HugoSmits86/nativewebp` | `v1.3.0` | MIT | Pure-Go WebP image encoder | https://github.com/HugoSmits86/nativewebp |
| `golang.org/x/crypto` | `v0.58.0` | BSD-3-Clause | `Argon2id`, `HKDF`, & cryptographic primitives | https://cs.opensource.google/go/x/crypto |
| `golang.org/x/image` | `v0.47.0` | BSD-3-Clause | Pure-Go WebP/BMP/TIFF decoding & image scaling | https://cs.opensource.google/go/x/image |
| `golang.org/x/net` | `v0.62.0` | BSD-3-Clause | HTML5 tokenizer, AST allowlist sanitizer & network utilities | https://cs.opensource.google/go/x/net |

> **MPL-2.0 Notice (`github.com/go-sql-driver/mysql`)**: `Volo-go` links the unmodified upstream `github.com/go-sql-driver/mysql` module via standard Go module imports. Source code for `github.com/go-sql-driver/mysql` is available at https://github.com/go-sql-driver/mysql under the Mozilla Public License 2.0.

---

## 2. Indirect Go Runtime Dependencies (`go.mod`) / Go 后端传递运行时依赖

| Module / 模块 | License / 许可证 | Transitive Consumer / 归属能力 | Upstream / 来源 |
| :--- | :--- | :--- | :--- |
| `filippo.io/edwards25519` | BSD-3-Clause | MySQL `caching_sha2_password` / Ed25519 authentication (`go-sql-driver/mysql`) | https://github.com/FiloSottile/edwards25519 |
| `modernc.org/libc` | BSD-3-Clause | Pure-Go C standard library runtime (`modernc.org/sqlite`) | https://gitlab.com/cznic/libc |
| `modernc.org/mathutil` | BSD-3-Clause | Pure-Go math utilities (`modernc.org/sqlite`) | https://gitlab.com/cznic/mathutil |
| `modernc.org/memory` | BSD-3-Clause | Arena memory allocator (`modernc.org/sqlite`) | https://gitlab.com/cznic/memory |
| `github.com/dustin/go-humanize` | MIT | Numeric formatting utilities (`modernc.org/sqlite`) | https://github.com/dustin/go-humanize |
| `github.com/google/uuid` | BSD-3-Clause | UUID utilities (`modernc.org/sqlite`) | https://github.com/google/uuid |
| `github.com/mattn/go-isatty` | MIT | Terminal detection (`modernc.org/sqlite`) | https://github.com/mattn/go-isatty |
| `github.com/ncruces/go-strftime` | MIT | `strftime` formatting (`modernc.org/sqlite`) | https://github.com/ncruces/go-strftime |
| `github.com/remyoudompheng/bigfft` | BSD-3-Clause | Arbitrary-precision arithmetic (`modernc.org/sqlite`) | https://github.com/remyoudompheng/bigfft |
| `golang.org/x/sys` | BSD-3-Clause | Low-level OS syscall primitives | https://cs.opensource.google/go/x/sys |

> **Note on Built-in Subsystems & Test-Only Packages**: The pure-Go PostgreSQL v3 wire driver (`internal/db/sqlx`), single-pass HTML5 AST allowlist sanitizer & server-side lexical syntax highlighter (`internal/render`), MaxMind MMDB / ip2region XDB decoders (`internal/security`), BFS-CSR Aho-Corasick word-filter engine (`pkg/wordfilter`), and the embedded ES6 plugin VM (`pkg/plugin`) are first-party pure-Go implementations under Apache-2.0. `github.com/stretchr/testify` (MIT) in `go.mod` is used exclusively in `_test.go` suites and is not linked into production binaries.

---

## 3. Embedded Admin Console Dependencies (`web/admin/package.json`) / 内嵌管理台前端依赖

| Package / 包名 | License / 许可证 | Purpose / 用途 | Upstream / 来源 |
| :--- | :--- | :--- | :--- |
| `preact` | MIT | Lightweight UI component runtime | https://github.com/preactjs/preact |
| `@preact/signals` | MIT | Fine-grained reactive state management | https://github.com/preactjs/signals |
| `@codemirror/state` | MIT | CodeMirror 6 immutable editor state | https://github.com/codemirror/state |
| `@codemirror/view` | MIT | CodeMirror 6 DOM viewport engine (powers `IR` / `WYSIWYG` / `SV` Studio Editor) | https://github.com/codemirror/view |
| `@codemirror/commands` | MIT | CodeMirror 6 editing commands & undo/redo history | https://github.com/codemirror/commands |

> **Frontend Build Toolchain (`devDependencies`, not shipped in runtime binary)**: `typescript` (Apache-2.0, https://github.com/microsoft/TypeScript) and `vite` (MIT, https://github.com/vitejs/vite).

---

## 4. On-Demand Browser Runtime Libraries (Lazy CDN) / 按需懒加载前端渲染库

Pages containing LaTeX math expressions or Mermaid diagrams dynamically load the following client-side libraries on demand (configurable or self-hostable via `reading.vendor_cdn_prefix`; zero network requests on pages without math/diagrams):
仅当页面包含 LaTeX 数学公式或 Mermaid 图表时，浏览器才会按需异步加载以下前端渲染库（可通过 `reading.vendor_cdn_prefix` 切换自建镜像源；普通文章页零外部请求）：

| Library / 组件 | License / 许可证 | Purpose / 用途 | Upstream / 来源 |
| :--- | :--- | :--- | :--- |
| `KaTeX` (`katex@0.16.11`) | MIT | Client-side LaTeX math typesetting | https://github.com/KaTeX/KaTeX |
| `Mermaid` (`mermaid@10`) | MIT | Client-side declarative diagram & flowchart rendering | https://github.com/mermaid-js/mermaid |

---

## 5. Linguistic Tables & Official Plugin Datasets / 语言映射表与官方插件词表数据

| Dataset / 数据集 | License / 许可证 | Purpose / 用途 | Upstream / 来源 |
| :--- | :--- | :--- | :--- |
| `houbb/sensitive-word` & `houbb/sensitive-word-data` | Apache-2.0 | Sanitized & categorized base dictionary in `plugins/sensitive-words` | https://github.com/houbb/sensitive-word |
| `BYVoid/OpenCC` (`TSCharacters.txt` via `houbb/opencc4j`) | Apache-2.0 | Static BMP Traditional-to-Simplified Hanzi folding table in `pkg/wordfilter` | https://github.com/BYVoid/OpenCC |

---

## 6. Optional Standalone Plugin Package (`plugins/kanbanniang`) / 可选独立发布插件包（看板娘）

> **Distribution Scope / 分发范围说明**: To keep core OS/arch binary bundles (`~8 MB`) and the Distroless container image minimal and purely Apache-2.0 compatible, `plugins/kanbanniang` is **excluded** from the main `Volo-go` binary archives and base container image, and is distributed separately as an optional standalone plugin archive (`plugin-kanbanniang-v2.0.2.zip` / `plugin-kanbanniang.zip`).
> 为保持核心跨平台发行包（`~8 MB`）与 Distroless 容器镜像精简纯粹，`plugins/kanbanniang` **不包含**在主程序压缩包与默认容器镜像中，而是作为可选独立插件包（`plugin-kanbanniang-v2.0.2.zip` / `plugin-kanbanniang.zip`）按需安装。

| Component / 组件 | License / 条款 | Purpose / 用途 | Reference / 说明 |
| :--- | :--- | :--- | :--- |
| `kanbanniang` controller & integration wrapper (adapted from `88250/solo` & `b3log/kanbanniang`) | MIT | Plugin controller, toolbar icons, tips structure, and Live2D integration wrapper | [`plugins/kanbanniang/LICENSE`](./plugins/kanbanniang/LICENSE) |
| Live2D Cubism 2.1 WebGL runtime (`plugins/kanbanniang/assets/live2d.js` core) & 26 bundled character models | Upstream Original Authors / Personal & Non-Commercial Terms | Local offline Live2D WebGL rendering runtime and character `.moc` / `.mtn` / texture / audio assets | [`plugins/kanbanniang/SOURCES.json`](./plugins/kanbanniang/SOURCES.json), [`plugins/kanbanniang/README.md`](./plugins/kanbanniang/README.md) |
