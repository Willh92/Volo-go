# Contributing to Volo-go / 贡献指南

[简体中文](#简体中文) | [English](#english)

---

## 简体中文

感谢你关注并参与 `Volo-go` 的开发！为确保代码库始终保持纯净、轻量与高安全水位，提交 Pull Request 前请遵循以下工程规范：

### 1. 开发环境准备

- **Go**: `1.27.3`（与 `go.mod` 对齐；生产构建全程要求 `CGO_ENABLED=0` 与 `GOEXPERIMENT=nojsonv2`，严禁引入任何 `cgo` 依赖）
- **Node.js**: `24 LTS`（仅用于构建 `web/admin` 内嵌管理台产物）

```bash
# 安装并构建管理台前端产物
npm --prefix web/admin ci
npm --prefix web/admin run build

# 纯 Go 静态编译与静态检查
CGO_ENABLED=0 GOEXPERIMENT=nojsonv2 go build -buildvcs=false -trimpath -gcflags="all=-l" -ldflags="-s -w" ./cmd/volo
CGO_ENABLED=0 go vet ./...
```

### 2. 架构与质量红线（不可退化）

1. **体积与内存硬预算**：
   - 跨 6 大静态发布目标平台（`linux/amd64`, `linux/arm64`, `darwin/amd64`, `darwin/arm64`, `windows/amd64`, `windows/arm64`）单二进制体积必须 **`< 20 MiB`**；
   - 空载与温热常驻内存 `VmRSS` 必须 **`< 35 MiB`**；
   - 内嵌管理台静态产物总 Gzip 体积必须 **`< 300 KB`**，且 CodeMirror 6 三模式编辑器核心（`IR` / `WYSIWYG` / `SV`）必须保持按需懒加载。
2. **多数据库方言与事务纪律**：
   - 所有 SQL 统一使用 `?` 占位符并通过 `sqlx.Store` / `Dialect` 自动重绑定，分页与 UPSERT 必须调用 `Dialect.PaginateSQL` 与 `Dialect.UpsertSQL`（兼容 `SQLite`, `PostgreSQL`, `MySQL`）；
   - 多表写入必须包裹在 `store.WithTx(ctx, ...)` 中，确保任一步失败自动全量回滚；
   - 只读请求严禁产生同步写库副作用（高频计数走异步合并缓冲 `WithBgTx`）。
3. **10 大出口隐私防线**：任何涉及文章、搜索、Feed、评论、反向链接、插件钩子或 HTTP 缓存/压缩的改动，必须补充对应单测确保密码文章与 `:::lock` 内容零泄露。
4. **双语国际化与文档同步**：新增界面文案请同步更新 `internal/i18n/builtin/zh-CN.json`、`internal/i18n/builtin/en-US.json` 及管理台 `web/admin/src/i18n.ts`；涉及部署、CLI、API、主题/插件或架构变更请同步更新根目录文档及 `docs/zh-CN/` 与 `docs/en-US/`。

### 3. 提交前本地全套校验

```bash
npm --prefix web/admin run build
CGO_ENABLED=0 go test -count=1 ./...
CGO_ENABLED=1 go test -race -count=1 ./...
CGO_ENABLED=0 go test -run=^$ -bench=. -benchmem -benchtime=100ms ./internal/render ./internal/visibility ./internal/db/sqlx ./internal/server
bash scripts/verify-binary-budget.sh
```

### 4. 开源协议声明

向本仓库提交贡献即表示您同意按照 [Apache License 2.0](./LICENSE) 第 5 条授权您的代码与文档贡献。

---

## English

Thank you for contributing to `Volo-go`! Please follow these engineering guidelines to keep the engine fast, self-contained, and secure.

### 1. Prerequisites

- **Go**: `1.27.3` (matching `go.mod`; `CGO_ENABLED=0` and `GOEXPERIMENT=nojsonv2` are mandatory for release builds; C/`cgo` dependencies are strictly disallowed)
- **Node.js**: `24 LTS` (used only to compile the embedded `web/admin` Studio Console)

```bash
# Install and build the embedded Admin Console assets
npm --prefix web/admin ci
npm --prefix web/admin run build

# Pure-Go static build and static analysis
CGO_ENABLED=0 GOEXPERIMENT=nojsonv2 go build -buildvcs=false -trimpath -gcflags="all=-l" -ldflags="-s -w" ./cmd/volo
CGO_ENABLED=0 go vet ./...
```

### 2. Non-Negotiable Architectural Invariants

1. **Footprint Budgets**:
   - Compiled binary size **`< 20 MiB`** across all 6 static release targets (`linux/amd64`, `linux/arm64`, `darwin/amd64`, `darwin/arm64`, `windows/amd64`, `windows/arm64`);
   - Idle & warm `VmRSS` **`< 35 MiB`**;
   - Embedded Admin SPA **`< 300 KB`** total gzipped, keeping the CodeMirror 6 3-mode editor core (`IR` / `WYSIWYG` / `SV`) lazy-loaded.
2. **SQL Dialect & Transaction Discipline**:
   - Write SQL with `?` placeholders routed through `sqlx.Store` / `Dialect`, using `Dialect.PaginateSQL` and `Dialect.UpsertSQL` for cross-DB portability (`SQLite`, `PostgreSQL`, `MySQL`);
   - Wrap all multi-table mutations in `store.WithTx(ctx, ...)` so partial failures roll back cleanly;
   - Keep read paths free of synchronous write locks (high-frequency counters use write-behind batching via `WithBgTx`).
3. **10-Surface Zero-Leak Perimeter**: Any change touching rendering, feeds, search, comments, backlinks, plugins, or HTTP caching/compression must include unit tests preserving 100% redaction of password-protected posts and locked `:::lock` blocks.
4. **Bilingual Parity**: Keep `internal/i18n/builtin/zh-CN.json`, `internal/i18n/builtin/en-US.json`, `web/admin/src/i18n.ts`, root docs, and `docs/{zh-CN,en-US}/` strictly synchronized.

### 3. Pre-PR Verification Suite

```bash
npm --prefix web/admin run build
CGO_ENABLED=0 go test -count=1 ./...
CGO_ENABLED=1 go test -race -count=1 ./...
CGO_ENABLED=0 go test -run=^$ -bench=. -benchmem -benchtime=100ms ./internal/render ./internal/visibility ./internal/db/sqlx ./internal/server
bash scripts/verify-binary-budget.sh
```

### 4. License

By submitting a contribution, you agree that your contribution is licensed under the [Apache License, Version 2.0](./LICENSE) (Section 5).
