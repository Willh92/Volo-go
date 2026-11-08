# Third-Party Open Source Notices / 第三方开源组件许可证声明

`Volo-go` is licensed under the **Apache License, Version 2.0** ([LICENSE](./LICENSE)).
`Volo-go` 以 **Apache License 2.0** 协议开源发布（详见 [LICENSE](./LICENSE)）。

Binary distributions and container images of `Volo-go` incorporate the following pure-Go (`CGO_ENABLED=0`) and frontend open-source components, all of which are compatible with Apache-2.0 redistribution:
`Volo-go` 的单文件静态二进制与容器镜像包含以下全部兼容 Apache-2.0 分发的纯 Go（`CGO_ENABLED=0`）与前端开源依赖组件：

---

## 1. Go Runtime & Backend Dependencies / Go 后端依赖

| Module / 模块 | License / 许可证 | Purpose / 用途 |Upstream / 来源 |
| :--- | :--- | :--- | :--- |
| `modernc.org/sqlite` | BSD-3-Clause | Pure-Go (`CGO_ENABLED=0`) SQLite engine | https://gitlab.com/cznic/sqlite |
| `github.com/jackc/pgx/v5` | MIT | PostgreSQL / Supabase / Neon / CockroachDB driver | https://github.com/jackc/pgx |
| `github.com/go-sql-driver/mysql` | MPL-2.0 | MySQL 8+ / MariaDB / TiDB pure-Go driver (unmodified module) | https://github.com/go-sql-driver/mysql |
| `github.com/yuin/goldmark` | MIT | CommonMark + GFM Markdown AST parser & renderer | https://github.com/yuin/goldmark |
| `github.com/yuin/goldmark-highlighting/v2` | MIT | Goldmark syntax highlighting integration | https://github.com/yuin/goldmark-highlighting |
| `github.com/alecthomas/chroma/v2` | MIT | Server-side lexical syntax highlighter | https://github.com/alecthomas/chroma |
| `github.com/microcosm-cc/bluemonday` | BSD-3-Clause | Strict HTML allowlist sanitizer (XSS defense) | https://github.com/microcosm-cc/bluemonday |
| `github.com/dop251/goja` | MIT | Pure-Go ECMAScript 5.1+ embedded plugin VM | https://github.com/dop251/goja |
| `github.com/Gen2Brain/webp` / `github.com/HugoSmits86/nativewebp` | MIT | Pure-Go WebP encoder/decoder | https://github.com/HugoSmits86/nativewebp |
| `github.com/lionsoul2014/ip2region/binding/golang` | Apache-2.0 | Offline `ip2region.xdb` vector-indexed reader | https://github.com/lionsoul2014/ip2region |
| `golang.org/x/crypto` | BSD-3-Clause | `Argon2id` & `PBKDF2` cryptographic primitives | https://cs.opensource.google/go/x/crypto |
| `golang.org/x/image` | BSD-3-Clause | Pure-Go image decoding & scaling | https://cs.opensource.google/go/x/image |
| `golang.org/x/net` | BSD-3-Clause | HTML5 tokenizer & network utilities | https://cs.opensource.google/go/x/net |
| `golang.org/x/text` | BSD-3-Clause | Unicode & language tag normalization | https://cs.opensource.google/go/x/text |

> **MPL-2.0 Notice (`github.com/go-sql-driver/mysql`)**: `Volo-go` links the unmodified upstream `github.com/go-sql-driver/mysql` module via standard Go module imports. Source code for `github.com/go-sql-driver/mysql` is available at https://github.com/go-sql-driver/mysql under the Mozilla Public License 2.0.

---

## 2. Embedded Admin Console Dependencies / 内嵌管理台前端依赖

| Package / 包名 | License / 许可证 | Purpose / 用途 | Upstream / 来源 |
| :--- | :--- | :--- | :--- |
| `preact` | MIT | Lightweight 3 KB UI runtime | https://github.com/preactjs/preact |
| `@preact/signals` | MIT | Fine-grained reactive state management | https://github.com/preactjs/signals |
| `@codemirror/state` | MIT | CodeMirror 6 immutable editor state | https://github.com/codemirror/state |
| `@codemirror/view` | MIT | CodeMirror 6 DOM viewport engine | https://github.com/codemirror/view |
| `@codemirror/commands` | MIT | CodeMirror 6 editing commands & history | https://github.com/codemirror/commands |
| `@codemirror/lang-markdown` & `@lezer/markdown` | MIT | Incremental Markdown grammar & parser | https://github.com/codemirror/lang-markdown |
