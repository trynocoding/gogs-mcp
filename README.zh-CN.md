# Gogs MCP

[English](README.md) | 简体中文

Gogs MCP 是一个本地 stdio MCP 服务器，对接 Gogs v0.14.2。默认只读：查看当前登录用户、浏览有权访问的仓库及其内容、读取 issue、审查 pull request。写 issue 的工具要显式设置 `GOGS_MCP_WRITE_ENABLED` 才注册。

| 工具 | 用途 |
|---|---|
| `get_authenticated_user` | 返回当前个人访问令牌对应的用户。 |
| `list_repositories` | 返回自有仓库和协作参与的仓库，按 `full_name` 排序，分页在客户端完成。 |
| `search_repositories` | 按名称、完整名称和描述搜索可访问的仓库，匹配在本地完成。 |
| `get_repository` | 返回仓库元数据，以及 pull、push、admin 三项权限。 |
| `list_directory` | 列出指定分支、标签或提交下的文件、目录、符号链接和子模块。 |
| `get_file` | 读取文本文件的指定行范围；二进制文件和超大文件只返回元数据，不返回内容。 |
| `list_branches` | 列出所有分支及其头提交 SHA，按名称排序，分页在客户端完成。 |
| `get_branch` | 返回单个分支及其头提交 SHA，分支名可以带斜杠。 |
| `list_commits` | 返回默认分支上最近的提交，每条只带消息的第一行。 |
| `get_commit` | 按 SHA 返回单个提交，包括作者、提交者、消息主题、父提交和 web URL。 |
| `search_code` | 在一份不可变的提交快照里搜索文件内容，支持字面量和正则两种模式，结果数、文件大小和耗时都有上限。 |
| `list_issues` | 按 Gogs 返回的顺序列出 open 或 closed 的 issue，包含编号、标题、创建者、标签、评论数和时间戳。 |
| `get_issue` | 返回一个 issue 的完整记录：正文、创建者、指派人、标签、里程碑、评论数、时间戳。 |
| `list_issue_comments` | 按创建顺序列出一个 issue 的评论，可以用 RFC3339 时间戳只取某个时刻之后的评论。 |
| `list_pull_requests` | 列出一个仓库的 pull request：标题、作者、状态、评论数、时间戳和头提交 SHA，数据来自 pull 引用和对应的 issue。 |
| `get_pull_request` | 返回单个 pull request 的正文、标签、头提交 SHA 和 web URL。目标分支默认取仓库默认分支，由 `base_ref_assumed` 标注。 |
| `get_pull_request_diff` | 渲染单个 pull request 的 merge-base diff：标准 unified 格式，带逐文件统计、提交列表、路径过滤、字节上限和启发式 merge state。 |
| `create_issue` | 创建 issue，标题必填，正文可选。仅在设置了 `GOGS_MCP_WRITE_ENABLED` 时注册。 |
| `update_issue` | 更新一个 issue 的标题、正文、状态、指派人或里程碑。仅在设置了 `GOGS_MCP_WRITE_ENABLED` 时注册。 |
| `create_issue_comment` | 为一个 issue 添加评论。仅在设置了 `GOGS_MCP_WRITE_ENABLED` 时注册。 |

## 环境要求

- Go 1.25。
- Gogs v0.14.2。
- 一个 Gogs 个人访问令牌。

## 构建与测试

```bash
task build
task test
task test:integration
task lint
```

构建产物写入 `.bin/gogs-mcp`。

## 构建离线包

```bash
task package
task verify:offline
```

`task package` 在 `dist/` 下生成 `gogs-mcp-<version>-fedora43-amd64-offline.tar.gz`，内含：静态链接的 Linux AMD64 二进制；带 vendored 依赖的完整源码归档；服务器自身的 OCI 容器镜像；版本锁定的 Gogs v0.14.2 E2E 镜像（本机有 docker 且对应版本的 Gogs checkout 可用时）；安装、卸载、镜像加载和离线验证脚本；产品文档；SHA-256 清单、CycloneDX SBOM、第三方许可证和 `SOURCE-METADATA.json`。二进制里写入了版本号、commit 和构建时间，`gogs-mcp version --json` 可以输出机器可读的版本信息。

`task verify:offline` 解开 bundle，在完全断网的前提下验证它可用：校验清单里的 SHA-256；确认二进制的架构和静态链接；比对二进制与源码归档的版本；存在 Gogs E2E 镜像时核对它的摘要；用占位配置过一遍 stdio MCP 协议；以 `GOTOOLCHAIN=local GOPROXY=off` 重建 vendored 源码；在无网络的 namespace 里把安装和卸载各跑一遍；本机有容器引擎时，导入 OCI 镜像并核对架构、容器用户和产品身份（全程禁止拉取）。

## 运行真实 Gogs 冒烟测试

冒烟测试需要一个运行中的 Docker 守护进程，以及旁边的 Gogs 源码 checkout——里面要有提交 `5dcb6c64bdf61e38dbdbb941c1d69789c560d0fb`（`v0.14.2`）。测试会把这个提交对应的源码树原样导出，构建一个隔离的测试镜像，建一套全新的 SQLite 数据库、用户和个人访问令牌，然后通过 stdio 调用 MCP 服务器。

```bash
task test:e2e:smoke
task test:e2e:repositories
task test:e2e:contents
task test:e2e:git
task test:e2e:search
task test:e2e:issues
task test:e2e:protocol
```

`task test:e2e` 一条命令跑完所有场景：仓库发现、源码浏览、代码搜索、issue 创建与维护、旧版 MCP initialize 握手，以及认证和权限失败路径。每个会话结束后会扫描日志，确认 token、私有源码和 issue 正文没有泄漏。

Gogs checkout 不在默认位置时，用 `GOGS_E2E_SOURCE_DIR` 指定路径。仓库 E2E 场景会创建一个所有者、一个只读协作者、一个外人，以及几座互相隔离的私有仓库，验证的是 Gogs 真实的可见性和权限行为。每次运行都随机分配主机端口、容器网络、镜像名和临时数据目录，跑完（不管成败）全部清理。

`list_directory` 和 `get_file` 接受可选的 `ref` 参数，值可以是分支、标签或提交 SHA；不传就先解析仓库默认分支。仓库内路径必须用正斜杠，不能是绝对路径，也不能出现 NUL、反斜杠、空路径段、`.` 或 `..`。

`get_file` 默认返回 200 行，一次最多 1000 行。文本会控制在 64 KiB 的结构化输出上限以内；还有更多内容时，响应带 `meta.next_start_line`，下次从这个行号接着取。超过 1 MiB 的文件和二进制文件只返回元数据，不返回内容；文本因超限被截断时，`meta.truncated` 会置位并附警告。

`list_commits` 只查默认分支——Gogs v0.14.2 的这个接口总是从 `HEAD` 开始，只支持 page size，没有别的选择。一次最多取 100 个提交，超过 200 字符的消息截短，`meta.truncated` 置位并附警告。Gogs v0.14.2 只给提交消息的第一行，所以 `get_commit` 返回的也是这一行，不做截断。`get_commit` 接受 Git 认可的任何单段 revision（完整或短 SHA、标签、不带斜杠的分支名）；Gogs 返回什么就透传什么——`git rev-parse` 不认的 revision 得到 not-found 错误，格式合法但不存在的 SHA 得到服务器错误。

`search_code` 的查询最长 1000 字符，可以指定 `ref`（分支、标签或提交 SHA）和各种边界参数。查询默认按字面量、区分大小写处理；`mode: "regex"` 切换成正则（RE2 语法），`case_sensitive: false` 关闭大小写敏感。查询或 glob 写法不合法时，直接返回 `INVALID_ARGUMENT`，不会去解析 ref，也不会下载归档。ref 会先解析成完整的提交 SHA（依次尝试分支、标签、revision），然后服务器从 Gogs 下载这个提交的 tar.gz 归档——归档对应固定的提交，搜索期间内容不可能变。归档解压到按用户/仓库/提交划分的快照缓存里，缓存只归当前用户所有（目录 `0700`，文件 `0600`）。解压严格限制在缓存根内：符号链接、硬链接和设备文件一律跳过，路径穿越直接拒绝。

所有结果边界都可观察：默认最多返回 50 个匹配（`max_results` 可以调到 500），每个匹配带文件路径、从 1 起算的行号和字节列号、匹配行，以及前后各两行上下文（`context_lines`，0 到 10）。命中 `max_results`、64 KiB 输出上限或搜索时限（`timeout_seconds`，1 到 300）任何一个，`meta.truncated` 都会置位并附警告。超时但已有部分结果时照常返回；一无所获的超时返回 `SEARCH_TIMEOUT`。`include` 和 `exclude` 的 glob 过滤路径，`.git` 始终排除。二进制文件（含 NUL 字节或无效 UTF-8）和超过 1 MiB 的文件（上限 `GOGS_MCP_MAX_FILE_BYTES`）会跳过，并在警告里说明。重复搜索同一个 commit 时，结果带 `meta.cache_hit`，不会再请求 Gogs。

快照缓存有上限：24 小时（`GOGS_MCP_CACHE_TTL`）没被用到的快照过期；每个用户的缓存超过 2 GiB（`GOGS_MCP_CACHE_MAX_BYTES`）时，先淘汰最近最少使用的快照，再开始新下载。正在被搜索使用的快照不会被淘汰；实在腾不出空间，这次搜索就以 `CACHE_CAPACITY_EXCEEDED` 失败，而不是继续下载。`gogs-mcp cache clean` 清掉当前用户的快照缓存和 pull request diff 缓存，`gogs-mcp cache clean --all` 清掉所有已验证的缓存根；配置、令牌和缓存根之外的任何东西都不会动。

Gogs 的 issue 只用于仓库内部讨论；需求仍以 Jira 为准，本服务器不与 Jira 同步。`list_issues` 只接受 `open` 和 `closed` 两个状态值——Gogs v0.14.2 会把其他值一律当成 open；page size 也不让传，服务端写死了。有没有下一页看 Gogs 的 `Link` 响应头，有的话通过 `meta.next_page` 告诉你。`list_issues` 返回紧凑摘要，不带正文；`get_issue` 返回完整记录，含正文、创建者、指派人、标签、里程碑、评论数和时间戳。找不到时统一返回同一个 not-found 错误码，不区分是仓库不存在还是 issue 不存在。`list_issue_comments` 先验证 `since` 是不是合法的 RFC3339 时间戳，再去请求 Gogs；结果受 `max_comments`（默认 100，最多 500）和 64 KiB 输出上限约束，碰到任一上限就置位 `meta.truncated` 并附警告。

Gogs v0.14.2 没有 pull request API。`list_pull_requests`、`get_pull_request` 和 `get_pull_request_diff` 用 Gogs 实际提供的东西拼出 pull request 数据：base 仓库上的 `refs/pull/{number}/head` 引用、底层的 issue，以及 git 协议。三个工具都是只读的，始终注册。

`list_pull_requests` 用 `ls-remote` 列出仓库的全部 pull 引用，新的在前，逐条拼上 issue 元数据。`state` 接受 `open`（默认）、`closed` 和 `all`；`limit` 默认 30，最多 100。每条结果都要查一次 issue，所以结果按 `limit` 截断，没有分页。

`get_pull_request` 补上正文、标签和 web URL，外加头提交 SHA。目标分支在 API 里拿不到，`base_ref` 填的是仓库默认分支，`base_ref_assumed` 为 `true`；pull request 实际指向别的分支时，给 diff 工具显式传 base。

`get_pull_request_diff` 渲染 merge-base diff：引擎把 pull 引用和 base 分支 fetch 到 `GOGS_MCP_CACHE_DIR` 下的裸缓存仓库，算出 merge base，再按标准 unified 格式编码。结果带每个文件的新增/删除行数、merge base 到头提交之间的提交列表和 merge-base SHA。`max_bytes` 限制渲染出的 diff 大小（默认 256 KiB，至多 4 MiB），触顶时 `meta.truncated` 置位并附警告。`base_ref` 可以覆盖假定的目标分支；没有 pull 引用的 issue 返回 `INVALID_ARGUMENT`。可选的 `paths` 列表把渲染出的 diff 和文件统计限制在匹配路径内——尾斜杠匹配整个目录——而 merge state 始终描述整个 pull request；一条路径都没匹配上时返回空 diff 并附警告。`merge_state` 在 base 分支没有越过 merge base 时报 `fast_forward`，两侧都有新提交但没碰同一个文件时报 `diverged`，两侧都改了同一个文件时报 `conflicting` 并把涉及路径列在 `merge_conflict_paths`。这个状态是从两侧改动文件列表推出的启发式，不是真的执行合并，警告里会说明。git 走 HTTP(S) 时用同一个令牌做 basic auth。

有些仓库把不同的项目放在不同的分支上，pull request 的目标分支可能既不是默认分支，和默认分支也毫无关系。Gogs 依旧不暴露目标分支，所以这类仓库必须显式传 `base_ref`：base 传错，diff 就是对着错误的历史渲染出来的。`base_commits` 字段报告所选 base 分支相对 merge base 前进了多少个提交；当 base 只是假设值时，数字很大就会触发警告明说这一点——零表示默认分支没有越过 merge base。

diff 缓存与快照缓存共用上限：24 小时（`GOGS_MCP_CACHE_TTL`）没被动过的仓库过期，缓存超过 2 GiB（`GOGS_MCP_CACHE_MAX_BYTES`）时按最近最少使用淘汰；正在 diff 的仓库只有在腾不出任何空间时才会被移除。`gogs-mcp cache clean` 会把它和快照缓存一起清掉。

`create_issue`、`update_issue` 和 `create_issue_comment` 仅在设置 `GOGS_MCP_WRITE_ENABLED` 时注册，所以默认服务器提供的工具清单是纯只读的。三个工具都不会自动重试：如果写请求已经到达 Gogs 但响应丢了，工具会报 `WRITE_OUTCOME_UNKNOWN`，这时应该去查 issue 或评论确认结果，而不是再写一次。创建普通 issue 只需要标题（1 到 255 字符），正文可选、最大 1 MiB；只要有读 issue 的权限就能建。要设置指派人、标签或里程碑，需要仓库的 push 权限，否则报 `PERMISSION_DENIED`——因为 Gogs 会悄悄丢弃无写权限用户的这些字段。指派人、每个标签名和里程碑标题会先验证存在，引用不存在时返回 `INVALID_ARGUMENT`。结果里有 issue 编号、状态和 web URL，可以用 `get_issue` 再查。

`update_issue` 要求 title、body、assignee、milestone、state 至少给一项，碰到空更新直接拒绝，不会请求 Gogs。省略的字段保持原值，写了值就是替换——所以空 body 表示清空正文，空 assignee 或 milestone 表示清除引用。Gogs 允许 issue 作者改自己 issue 的标题、正文和状态；没有写权限的人做其他任何更新都会被 `PERMISSION_DENIED` 拒绝。改指派人或里程碑还要有仓库 push 权限，且引用必须存在——发送前会先验证。评论正文最少 1 字节、最多 1 MiB；结果带评论 ID、作者、正文和时间戳。

## 配置凭据

建议把令牌放进私有文件，免得它留在 shell 历史或 Claude Code 配置里。

```bash
install -m 0600 /dev/null "$HOME/.config/gogs-mcp/token"
printf '%s\n' 'YOUR_GOGS_PAT' >"$HOME/.config/gogs-mcp/token"
chmod 0600 "$HOME/.config/gogs-mcp/token"
```

令牌文件权限比 `0600` 更宽，服务器会直接拒绝。`GOGS_TOKEN` 和 `GOGS_TOKEN_FILE` 只能二选一。要连纯 HTTP，必须显式设 `GOGS_ALLOW_INSECURE_HTTP=true`，否则拒绝连接。

## 验证连接

```bash
GOGS_BASE_URL=https://gogs.internal.example/ \
GOGS_TOKEN_FILE="$HOME/.config/gogs-mcp/token" \
.bin/gogs-mcp verify
```

命令输出机器可读的 JSON，不含任何凭据。成功时结果里带当前用户信息。

## 配置 Claude Code

构建二进制，然后把它注册成用户级 stdio 服务器。

```bash
claude mcp add gogs \
  --scope user \
  --transport stdio \
  --env GOGS_BASE_URL=https://gogs.internal.example/ \
  --env GOGS_TOKEN_FILE="$HOME/.config/gogs-mcp/token" \
  -- /absolute/path/to/gogs-mcp/.bin/gogs-mcp serve
```

用 `claude mcp get gogs` 或 Claude Code 里的 `/mcp` 检查连接状态。`serve` 的 stdout 只跑 MCP 协议消息，JSON 日志全部走 stderr。

## 配置

可选的 JSON 文件存放非敏感设置：

```bash
.bin/gogs-mcp serve --config config.example.json
```

环境变量优先于文件值。

| 环境变量 | 默认值 | 描述 |
|---|---|---|
| `GOGS_BASE_URL` | 无。 | 必填，Gogs 的基础 URL，已有子路径会保留。 |
| `GOGS_TOKEN` | 无。 | 直接传入的个人访问令牌。 |
| `GOGS_TOKEN_FILE` | 无。 | 私有令牌文件的路径。 |
| `GOGS_CA_FILE` | 系统信任库。 | 额外的 PEM CA 证书文件。 |
| `GOGS_ALLOW_INSECURE_HTTP` | `false`。 | 显式允许纯 HTTP。 |
| `GOGS_MCP_CACHE_DIR` | OS 用户缓存目录。 | `search_code` 快照与 pull request diff 缓存根的绝对路径。 |
| `GOGS_MCP_CACHE_MAX_BYTES` | `2147483648`。 | 每个用户缓存的容量上限，超过后按最近最少使用淘汰；覆盖 search 快照和 pull request diff。 |
| `GOGS_MCP_CACHE_TTL` | `24h`。 | 快照或 pull request 缓存条目多久没被使用就过期。 |
| `GOGS_MCP_SEARCH_TIMEOUT` | `30s`。 | `search_code` 的默认搜索时限，至多 `5m`。 |
| `GOGS_MCP_MAX_FILE_BYTES` | `1048576`。 | 超过这个大小的文件会被 `search_code` 跳过。 |
| `GOGS_MCP_WRITE_ENABLED` | `false`。 | 注册 `create_issue`、`update_issue` 和 `create_issue_comment`。 |
| `GOGS_HTTP_TIMEOUT` | `30s`。 | HTTP 请求超时。 |
| `GOGS_LOG_LEVEL` | `info`。 | `debug`、`info`、`warn` 或 `error`。 |

标准的 `HTTP_PROXY`、`HTTPS_PROXY` 和 `NO_PROXY` 变量照常生效。

## 命令

```text
gogs-mcp serve [--config PATH]
gogs-mcp verify [--config PATH]
gogs-mcp cache clean [--config PATH] [--all]
gogs-mcp version [--json]
```

退出码约定：配置错误为 2；`verify` 遇到连接、认证、TLS、超时错误为 3；其他内部错误为 1。
