## Summary / 变更概述

<!-- Briefly describe what changed and why / 简述本次变更解决的问题与设计动机 -->

## Related Issue / 关联 Issue

Closes #

## Verification Checklist / 验收自检清单

- [ ] **Pure Go (`CGO_ENABLED=0`)**: `CGO_ENABLED=0 go build ./...` & `go vet ./...` pass cleanly
- [ ] **Tests & Race Detector**: `CGO_ENABLED=0 go test -count=1 ./...` & `CGO_ENABLED=1 go test -race -count=1 ./...` pass
- [ ] **Binary & Memory Budgets**: `bash scripts/verify-binary-budget.sh` confirms binary `< 20 MiB` and live RSS `< 35 MiB`
- [ ] **10-Surface Privacy Perimeter**: No leakage of password-protected articles or locked `:::lock` blocks across cards, feeds, search, sitemap, backlinks, `/llms.txt`, or HTTP compression
- [ ] **Multi-Dialect SQL**: Queries use `Dialect` (`?` rebound via `sqlx`, portable `UpsertSQL` / `PaginateSQL`), multi-table writes use `WithTx`, and read queries perform zero DB writes
- [ ] **Bilingual Docs & i18n**: Updated `zh-CN` and `en-US` locale keys / documentation where applicable
