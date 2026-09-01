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
| `search_code` | Search for a case-sensitive literal string across the files of an immutable commit snapshot. |

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
```

Set `GOGS_E2E_SOURCE_DIR` when the Gogs checkout is stored elsewhere. The repository E2E scenario creates an owner, a read-only collaborator, an outsider, and isolated private repositories to verify actual Gogs visibility and permission behavior. Every run uses a random host port, container network, image name, and temporary data directory. The tests remove all of them after success or failure.

`list_directory` and `get_file` accept an optional `ref` containing a branch, tag, or commit SHA. When it is omitted, the repository default branch is resolved first. Repository paths must be canonical forward-slash paths and cannot be absolute or contain NUL, backslashes, empty components, `.` or `..` components.

`get_file` returns 200 lines by default and accepts at most 1000 lines. Text output is kept below the 64 KiB structured-output limit and includes `meta.next_start_line` when another line-range call can continue. Files larger than 1 MiB and binary files return metadata without their payload; oversized text results set `meta.truncated` and include a warning.

`list_commits` is fixed to the default branch because Gogs v0.14.2 always starts at `HEAD` and supports only a page size. It accepts at most 100 commits, shortens messages longer than 200 characters, and sets `meta.truncated` with a warning. Gogs v0.14.2 only exposes the first line of a commit message, so `get_commit` returns that same line untruncated. `get_commit` accepts any single-segment revision Git understands (full or short SHA, tag, branch name without slashes); unknown revisions surface the Gogs response verbatim, which is a not-found error for revisions `git rev-parse` rejects and a server error for well-formed SHAs that do not exist.

`search_code` accepts a case-sensitive literal query of at most 1000 characters and an optional `ref` containing a branch, tag, or commit SHA. The ref is resolved to a full commit SHA (branch first, then tag, then revision), and the server downloads the tar.gz archive of that exact commit from Gogs, which cannot change while the search runs. The archive is extracted into a per-user, per-repository, per-commit snapshot cache owned only by the current user (directories `0700`, files `0600`), and archives are unpacked strictly inside the cache root: symlinks, hardlinks, and device entries are skipped, and path traversal is rejected. Up to 50 matches are returned with file path, 1-based line and byte column, the matching line, and two lines of context before and after; oversized output sets `meta.truncated` with a warning. Repeat searches for the same commit set `meta.cache_hit` and do not contact Gogs again.

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
| `GOGS_MCP_WRITE_ENABLED` | `false`. | Reserved for optional write tools in later deliveries. |
| `GOGS_HTTP_TIMEOUT` | `30s`. | HTTP request timeout. |
| `GOGS_LOG_LEVEL` | `info`. | `debug`, `info`, `warn`, or `error`. |

The standard `HTTP_PROXY`, `HTTPS_PROXY`, and `NO_PROXY` variables are honored.

## Commands

```text
gogs-mcp serve [--config PATH]
gogs-mcp verify [--config PATH]
gogs-mcp version [--json]
```

Configuration failures exit with status 2. Connection, authentication, TLS, and timeout failures from `verify` exit with status 3. Other internal failures exit with status 1.
