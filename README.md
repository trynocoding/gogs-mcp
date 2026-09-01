# Gogs MCP

Gogs MCP is a local stdio MCP server for Gogs v0.14.2. It exposes read-only tools for inspecting the authenticated user and discovering every repository available to that user.

| Tool | Purpose |
|---|---|
| `get_authenticated_user` | Return the user associated with the configured personal access token. |
| `list_repositories` | Return owned and collaborator repositories in stable `full_name` order with client-side pagination. |
| `search_repositories` | Search accessible repository names, full names, and descriptions locally. |
| `get_repository` | Return repository metadata and pull, push, and admin permissions. |
| `list_directory` | List files, directories, symlinks, and submodules at a branch, tag, or commit. |
| `get_file` | Read a bounded text line range or return safe binary and oversized-file metadata. |
| `list_branches` | List every branch with its head commit SHA in stable name order with client-side pagination. |
| `get_branch` | Return a single branch and its head commit SHA. Branch names may contain slashes. |
| `list_commits` | Return the most recent commits of the default branch with the first line of each message. |
| `get_commit` | Return a single commit by SHA with author, committer, message subject, parents, and web URL. |
| `search_code` | Search file content of an immutable commit snapshot with a literal or regular-expression query, bounded by result count, file size, and timeout. |
| `list_issues` | List open or closed issues with number, title, creator, labels, comment count, and timestamps, following the Gogs page order. |
| `get_issue` | Return one issue with body, creator, assignee, labels, milestone, comment count, and timestamps. |
| `list_issue_comments` | List the comments of one issue in creation order, optionally restricted to comments since an RFC3339 timestamp. |
| `create_issue` | Create an issue with a title and an optional body. Only registered when `GOGS_MCP_WRITE_ENABLED` is set. |
| `update_issue` | Update the title, body, state, assignee, or milestone of one issue. Only registered when `GOGS_MCP_WRITE_ENABLED` is set. |
| `create_issue_comment` | Add a comment to one issue. Only registered when `GOGS_MCP_WRITE_ENABLED` is set. |

## Requirements

- Go 1.25.
- Gogs v0.14.2.
- A Gogs personal access token.

## Build and test

```bash
task build
task test
task test:integration
task lint
```

The binary is written to `.bin/gogs-mcp`.

## Run the real Gogs smoke test

The smoke test requires a running Docker daemon and a sibling Gogs checkout containing commit `5dcb6c64bdf61e38dbdbb941c1d69789c560d0fb` (`v0.14.2`). It exports that exact committed tree, builds an isolated test image, creates a fresh SQLite database, user, and personal access token, and then invokes the MCP server through stdio.

```bash
task test:e2e:smoke
task test:e2e:repositories
task test:e2e:contents
task test:e2e:git
task test:e2e:search
task test:e2e:issues
```

Set `GOGS_E2E_SOURCE_DIR` when the Gogs checkout is stored elsewhere. The repository E2E scenario creates an owner, a read-only collaborator, an outsider, and isolated private repositories to verify actual Gogs visibility and permission behavior. Every run uses a random host port, container network, image name, and temporary data directory. The tests remove all of them after success or failure.

`list_directory` and `get_file` accept an optional `ref` containing a branch, tag, or commit SHA. When it is omitted, the repository default branch is resolved first. Repository paths must be canonical forward-slash paths and cannot be absolute or contain NUL, backslashes, empty components, `.` or `..` components.

`get_file` returns 200 lines by default and accepts at most 1000 lines. Text output is kept below the 64 KiB structured-output limit and includes `meta.next_start_line` when another line-range call can continue. Files larger than 1 MiB and binary files return metadata without their payload; oversized text results set `meta.truncated` and include a warning.

`list_commits` is fixed to the default branch because Gogs v0.14.2 always starts at `HEAD` and supports only a page size. It accepts at most 100 commits, shortens messages longer than 200 characters, and sets `meta.truncated` with a warning. Gogs v0.14.2 only exposes the first line of a commit message, so `get_commit` returns that same line untruncated. `get_commit` accepts any single-segment revision Git understands (full or short SHA, tag, branch name without slashes); unknown revisions surface the Gogs response verbatim, which is a not-found error for revisions `git rev-parse` rejects and a server error for well-formed SHAs that do not exist.

`search_code` accepts a query of at most 1000 characters, an optional `ref` containing a branch, tag, or commit SHA, and optional bounds. The query is a case-sensitive literal string by default; `mode: "regex"` interprets it as a regular expression (RE2) and `case_sensitive: false` folds case. Invalid queries and globs are rejected with `INVALID_ARGUMENT` before the ref is resolved or any archive is downloaded. The ref is resolved to a full commit SHA (branch first, then tag, then revision), and the server downloads the tar.gz archive of that exact commit from Gogs, which cannot change while the search runs. The archive is extracted into a per-user, per-repository, per-commit snapshot cache owned only by the current user (directories `0700`, files `0600`), and archives are unpacked strictly inside the cache root: symlinks, hardlinks, and device entries are skipped, and path traversal is rejected.

Results are bounded and every bound is observable. Up to 50 matches are returned (at most 500 via `max_results`) with file path, 1-based line and byte column, the matching line, and two lines of context before and after (`context_lines`, 0 through 10). Reaching `max_results`, the 64 KiB structured-output limit, or the search time limit (`timeout_seconds`, 1 through 300) sets `meta.truncated` with a warning; a timeout with partial results returns them, and a timeout without results returns `SEARCH_TIMEOUT`. The `include` and `exclude` glob patterns restrict or skip paths, and always exclude `.git`. Binary files (a NUL byte or invalid UTF-8) and files larger than 1 MiB (`GOGS_MCP_MAX_FILE_BYTES`) are skipped and reported in warnings. Repeat searches for the same commit set `meta.cache_hit` and do not contact Gogs again.

The snapshot cache is bounded: snapshots untouched for 24 hours (`GOGS_MCP_CACHE_TTL`) are expired and the least recently used snapshots are removed when the per-user cache exceeds 2 GiB (`GOGS_MCP_CACHE_MAX_BYTES`), before a new download starts. Snapshots held by an active search are never evicted; when no room can be made, the search fails with `CACHE_CAPACITY_EXCEEDED` instead of downloading. `gogs-mcp cache clean` removes the authenticated user's snapshot cache and `gogs-mcp cache clean --all` removes every verified cache root; configuration, tokens, and anything outside the cache root are never touched.

Gogs issues track repository-internal discussion; Jira remains the requirements system of record, and this server does not sync with Jira. `list_issues` accepts only the `open` and `closed` states because Gogs v0.14.2 treats every other value as open, and it does not accept a page size because Gogs v0.14.2 fixes it server-side. The next page comes from the Gogs `Link` header and is reported as `meta.next_page` only when one exists. `list_issues` returns compact summaries without bodies; `get_issue` returns the full record with body, creator, assignee, labels, milestone, comment count, and timestamps, and reports the shared not-found code without revealing whether the repository or the issue exists. `list_issue_comments` validates the RFC3339 `since` timestamp before contacting Gogs, and bounds the result with `max_comments` (default 100, at most 500) and the 64 KiB structured-output limit; reaching either bound sets `meta.truncated` with a warning.

`create_issue`, `update_issue`, and `create_issue_comment` are only registered when `GOGS_MCP_WRITE_ENABLED` is set, so the default server advertises exactly the read-only tool list. None of them retries its write: when the response is lost after the write reached Gogs, the tool reports `WRITE_OUTCOME_UNKNOWN` and the issue or comment must be looked up instead of repeated. A plain issue needs only a title (1 through 255 characters) and an optional body of at most 1 MiB, and every user with issue-read access can create one. An assignee, labels, or a milestone requires repository push permission (`PERMISSION_DENIED` otherwise), because Gogs silently drops those fields for users without write access; the assignee, every label name, and the milestone title are verified to exist before the issue is created, and unknown references are rejected with `INVALID_ARGUMENT`. The result carries the issue number, its state, and its web URL, which `get_issue` can return.

`update_issue` requires at least one of title, body, assignee, milestone, or state and rejects an empty update before contacting Gogs. Omitted fields keep their current value, while an explicit value replaces it, so an empty body clears the body and an empty assignee or milestone clears the reference. Gogs lets the issue author change the title, body, and state of their own issue and rejects every other update from a user without write access with `PERMISSION_DENIED`; changing the assignee or milestone additionally requires repository push permission and an existing reference, which are verified before the update is sent. Comment bodies must be between 1 byte and 1 MiB, and the comment result carries its ID, author, body, and timestamps.

## Configure credentials

Prefer a private token file so the token is not stored in shell history or Claude Code configuration.

```bash
install -m 0600 /dev/null "$HOME/.config/gogs-mcp/token"
printf '%s\n' 'YOUR_GOGS_PAT' >"$HOME/.config/gogs-mcp/token"
chmod 0600 "$HOME/.config/gogs-mcp/token"
```

The server rejects token files whose permissions are wider than `0600`. `GOGS_TOKEN` and `GOGS_TOKEN_FILE` are mutually exclusive. Plain HTTP is rejected unless `GOGS_ALLOW_INSECURE_HTTP=true` is explicitly set.

## Verify the connection

```bash
GOGS_BASE_URL=https://gogs.internal.example/ \
GOGS_TOKEN_FILE="$HOME/.config/gogs-mcp/token" \
.bin/gogs-mcp verify
```

The command prints machine-readable JSON without credentials. A successful result includes the authenticated user.

## Configure Claude Code

Build the binary and register it as a user-scoped stdio server.

```bash
claude mcp add \
  --scope user \
  --transport stdio \
  --env GOGS_BASE_URL=https://gogs.internal.example/ \
  --env GOGS_TOKEN_FILE="$HOME/.config/gogs-mcp/token" \
  gogs \
  -- /absolute/path/to/gogs-mcp/.bin/gogs-mcp serve
```

Use `claude mcp get gogs` or `/mcp` in Claude Code to check the connection. The `serve` command reserves stdout for MCP protocol messages and writes JSON logs only to stderr.

## Configuration

An optional JSON file may contain non-sensitive settings:

```bash
.bin/gogs-mcp serve --config config.example.json
```

Environment variables override file values.

| Environment variable | Default | Description |
|---|---|---|
| `GOGS_BASE_URL` | None. | Required Gogs base URL. An existing subpath is preserved. |
| `GOGS_TOKEN` | None. | Personal access token supplied directly. |
| `GOGS_TOKEN_FILE` | None. | Path to a private token file. |
| `GOGS_CA_FILE` | System trust store. | Additional PEM CA certificate file. |
| `GOGS_ALLOW_INSECURE_HTTP` | `false`. | Explicitly permits plain HTTP. |
| `GOGS_MCP_CACHE_DIR` | OS user cache directory. | Absolute path of the snapshot cache root for `search_code`. |
| `GOGS_MCP_CACHE_MAX_BYTES` | `2147483648`. | Snapshot cache size per Gogs user before least-recently-used eviction. |
| `GOGS_MCP_CACHE_TTL` | `24h`. | How long a snapshot may stay untouched before it expires. |
| `GOGS_MCP_SEARCH_TIMEOUT` | `30s`. | Default search time limit for `search_code`, at most `5m`. |
| `GOGS_MCP_MAX_FILE_BYTES` | `1048576`. | Files larger than this are skipped by `search_code`. |
| `GOGS_MCP_WRITE_ENABLED` | `false`. | Register `create_issue`, `update_issue`, and `create_issue_comment`. |
| `GOGS_HTTP_TIMEOUT` | `30s`. | HTTP request timeout. |
| `GOGS_LOG_LEVEL` | `info`. | `debug`, `info`, `warn`, or `error`. |

The standard `HTTP_PROXY`, `HTTPS_PROXY`, and `NO_PROXY` variables are honored.

## Commands

```text
gogs-mcp serve [--config PATH]
gogs-mcp verify [--config PATH]
gogs-mcp cache clean [--config PATH] [--all]
gogs-mcp version [--json]
```

Configuration failures exit with status 2. Connection, authentication, TLS, and timeout failures from `verify` exit with status 3. Other internal failures exit with status 1.
