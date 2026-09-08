# 架构审查修复与回归覆盖

本次修复对应架构与功能审查的 9 个问题，保持 stdio / Streamable HTTP、17 个只读工具及 3 个可选 Issue 写工具的范围。

| 问题 | 修复 | 主要回归位置 |
| --- | --- | --- |
| 快照只在下载前检查容量，Git 使用独立预算 | 快照、临时文件和 Git 共用用户预算，文件增长前预留，容量不足拒绝写入 | `internal/snapshot/audit_regression_test.go`、`internal/gogs/cache_regression_test.go`、`internal/diskcache/cache_test.go` |
| 同一用户的不同令牌实例同时修改共享 Git 引用 | 用户写锁及条目文件锁协调不同客户端和进程；读取期间固定缓存 | `internal/gogs/audit_regression_test.go`、`internal/diskcache/cache_test.go` |
| PR Git 未继承 CA 和超时 | REST / Git 使用一致的私有 CA 和请求超时，等待锁也可取消 | `internal/gogs/audit_regression_test.go` |
| Contents 目录响应包含文件正文，大文件突破响应上限 | Git tree 预检路径、固定提交 SHA，只对当前页读取 Contents 元数据前缀 | `internal/gogs/content_test.go`、`tests/integration/repositories_test.go`、`tests/e2e/contents_test.go` |
| 遇到 merge base 就终止遍历，遗漏合并旁支 | 按可达集合排除 merge base 全部祖先，保留旁支提交 | `internal/gogs/audit_regression_test.go` |
| diff 先构造完整 patch 再截断，搜索丢失取消信号 | 限制提交图与 blob 输入，逐文件生成有界 diff；共享执行槽、总时限并保留取消 | `internal/gogs/audit_regression_test.go`、`internal/mcpserver/audit_regression_test.go` |
| 认证缓存命中不断续期 | TTL 从认证成功计时；下游 401 立即失效缓存 | `internal/httpserver/audit_regression_test.go` |
| PR 仅限制返回数量，过滤时无限扫描 | 每次最多查 100 个 issue，以 `next_before` 继续，标明 total 统计范围 | `internal/gogs/audit_regression_test.go` |
| 输出上限不一致，第一条超长评论被丢弃且无法续读 | 统一输出预算；正文 UTF-8 分块、评论预览及游标；保留写入资源标识 | `internal/mcpserver/audit_regression_test.go` |

复查补测：崩溃遗留的用户临时解包目录一小时后清理；锁文件位于可写缓存卷；清理保留锁 inode；`snapshot-demo` / `project.git` 仓库名不能绕过快照锁；输出裁剪不修改资源路径、不删除分页成员；元数据超限返回带 request ID 的 `RESPONSE_TOO_LARGE` 工具结果；写请求取消不覆盖 `WRITE_OUTCOME_UNKNOWN`；按旧路径或新路径过滤时保留完整重命名信息，重命名候选也计入 diff 输入预算。

验证入口：

```bash
task test              # 全包单元测试、race 与覆盖率
task test:integration  # Gogs API 契约及 HTTP 隔离
task lint
task test:e2e          # 本地 Docker 中真实 Gogs v0.14.2
```

E2E 包含 Smoke、Repositories、Contents、Git、Pulls、Search、Issues、IssueWriting、LegacyInitializeProtocol 和 HTTPTransport。单元回归使用临时 Git 仓库、独立子进程和 HTTP 测试服务器复现容量、并发、取消、认证和输出边界；它们不等同于这些极限场景的真实 Gogs E2E。

缓存按实例、用户限额，不是整个部署的磁盘总限额。清理后保留空锁文件及父目录。正文游标针对当前文本，跨请求编辑可能改变内容。超出 diff 输入边界会明确报错；具体限制与续读参数见 README。
