# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## 项目概述

Gogs MCP 是一个本地 stdio MCP 服务器,针对 Gogs v0.14.2 的 API 契约编写。默认只注册只读工具;设置 `GOGS_MCP_WRITE_ENABLED` 后才额外注册 `create_issue`、`update_issue`、`create_issue_comment`。需要 Go 1.25 与 [task](https://taskfile.dev)。

## 常用命令

```bash
task build             # 构建到 .bin/gogs-mcp
task test              # go test -cover -race ./...(单元 + 协议测试)
task lint              # golangci-lint run
task test:integration  # HTTP 契约测试(tests/integration,httptest 假 Gogs)
task package           # 组装 dist/ 离线包(静态二进制 + vendored 源码 + OCI 镜像 + SBOM)
task verify:offline    # 无网络环境下验证离线包可重建、可安装、可运行
```

运行单个单元测试(不需要 build tag):

```bash
go test -race ./internal/mcpserver -run '^TestUpdateIssue$'
```

E2E 测试需要 Docker 和 Gogs v0.14.2 的同级 checkout(默认 `../gogs`,可用 `GOGS_E2E_SOURCE_DIR` 覆盖;每个场景 25 分钟超时):

```bash
task test:e2e:smoke    # 也可用 repositories / contents / git / search / issues / protocol 单跑一个场景
task test:e2e          # 全部场景
```

等价的原始命令形如 `go test -tags=e2e -count=1 -timeout=25m -v ./tests/e2e -run '^TestSmoke$'`。

运行服务器与验证连接:

```bash
.bin/gogs-mcp serve --config config.example.json
.bin/gogs-mcp verify   # 机器可读 JSON;配置错误退出码 2,连接错误退出码 3,其他内部错误 1
```

## 分层架构

请求路径:`cmd/gogs-mcp`(薄 main)→ `internal/app`(CLI 分发 serve/verify/cache/version 与退出码)→ `internal/mcpserver`(基于 modelcontextprotocol/go-sdk 的 MCP 工具注册,stdio 传输)→ `internal/gogs`(Gogs v0.14.2 REST 客户端)。

- `internal/mcpserver` — 按域注册工具:`repositories.go`、`contents.go`、`git.go`、`search.go`、`issues.go`。`server.go` 中的 `Client` 接口由 `*gogs.Client` 满足(消费方定义接口);新增工具时需同时扩展该接口与 `internal/gogs` 客户端。
- `internal/gogs` — HTTP 客户端与错误分类。`errors.go` 把传输错误与 HTTP 状态映射为稳定错误码(如 404 → `RESOURCE_NOT_FOUND_OR_FORBIDDEN`,不泄漏资源存在性);写操作的传输失败映射为 `WRITE_OUTCOME_UNKNOWN`。
- `internal/config` — 环境变量 + 可选 JSON 配置文件,环境变量优先;token 文件权限宽于 0600 即拒绝;`GOGS_TOKEN` 与 `GOGS_TOKEN_FILE` 互斥;纯 HTTP 必须显式 `GOGS_ALLOW_INSECURE_HTTP=true`。
- `internal/snapshot` — `search_code` 的 tar.gz 归档快照缓存:按用户/仓库/commit 缓存,LRU + TTL 淘汰(`eviction.go`),解包时跳过 symlink/hardlink/device 并拒绝路径穿越。
- `internal/securelog` — slog JSON handler,把 token 出现处替换为 `[REDACTED]`。日志只写 stderr;stdout 保留给 MCP 协议。
- `internal/packagegen` + `cmd/mkpackage` — 离线包元数据(SBOM、第三方许可、MANIFEST.sha256、SOURCE-METADATA.json);Taskfile 负责其余打包步骤,并注入 `internal/version` 的 ldflags。
- `vendor/` 已提交入库(`task vendor` 刷新),离线重建依赖它(`GOPROXY=off`),不要随手删除。

## 工具实现的固定模式

每个工具 handler 遵循相同模式(见 `server.go` 与各域文件):

1. `newRequestID()` 并用 `gogs.WithRequestMetadata(ctx, ...)` 附加到 context。
2. 调用 client;失败时用 `gogs.AsError(err)` 分类,把 `ToolError{Code, Message, Retryable}` 放进 `ToolResponse[T].Error` 并返回 `IsError: true` 的 result,不返回 Go error。
3. 输出统一包在 `ToolResponse[T]{data, error, meta}`(`response.go`),受 64 KiB 结构化输出上限约束;触顶或到达分页上限时设置 `meta.truncated` / `meta.next_page` / `meta.next_start_line` / `meta.warnings`。

写工具从不自动重试:响应丢失时报 `WRITE_OUTCOME_UNKNOWN`,由调用方查询结果而不是重复写入。

## Gogs v0.14.2 契约约束

代码中若干"看似奇怪"的行为是对 Gogs v0.14.2 的忠实映射,不要"修复":

- `list_commits` 固定走默认分支(Gogs v0.14.2 总是从 HEAD 开始,只支持 page size),且 commit message 只暴露第一行。
- `list_issues` 只接受 `open`/`closed` 两种状态、不接受 page size(服务端固定),下一页来自 `Link` 头。
- 404 一律报告共享的 `RESOURCE_NOT_FOUND_OR_FORBIDDEN`,不区分资源不存在与无权限。
- 分配 assignee/label/milestone 需要仓库 push 权限(Gogs 会静默丢弃无权限用户的这些字段),因此写工具先验证引用存在再发送。

`tests/integration` 用 httptest 服务器逐条断言这些 wire 契约(请求路径、query、Authorization 头等),改动工具行为时同步更新对应契约测试。

## 构建标签与 lint

- `integration` 与 `e2e` 是 build tag:普通 `go test ./...` 只跑单元测试;`.golangci.yml` 配置为带着这两个 tag 运行,所以 lint 覆盖全部测试代码。
- Taskfile 固定 `GOTOOLCHAIN: go1.25.0`;打包用 `CGO_ENABLED=0 GOOS=linux GOARCH=amd64` 静态构建。
- E2E 套件在每个会话结束后扫描日志,证明 token、私有源码与 issue 正文没有泄漏(`assertNoSecrets`);新增工具输出路径时保持这一保证。
