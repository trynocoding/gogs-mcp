# Troubleshooting

## The server fails to start in Claude Code

Run the same binary and environment by hand to see the error:

```bash
GOGS_BASE_URL="https://gogs.internal.example/" \
GOGS_TOKEN_FILE="$HOME/.config/gogs-mcp/token" \
~/.local/bin/gogs-mcp verify
```

Configuration problems exit with status 2, connection, authentication, TLS,
and timeout problems with status 3.

## The token file is rejected

`GOGS_TOKEN_FILE permissions must not be wider than 0600` means the token
file is readable by other users. Fix it with `chmod 0600` on the file. The
file must also be a regular file, not a named pipe or a symlink into a
world-readable directory.

## Authentication fails

`AUTHENTICATION_FAILED` from a tool call means Gogs rejected the token. The
token may have been revoked or expired; generate a new one as described in
`pat-management.md` and restart the MCP session.

## TLS errors with an internal Gogs

`TLS_ERROR` usually means the server certificate was issued by a company CA
that the local trust store does not know. Point `GOGS_CA_FILE` at the CA
certificate as described in `private-ca.md`.

## Connection or timeout errors

`CONNECTION_FAILED` and `TIMEOUT` mean the server could not reach Gogs.
Check that the base URL is reachable from this machine, that a required
proxy is configured (`HTTPS_PROXY`, `NO_PROXY`), and that the URL includes
the scheme (`https://`).

## A write reports WRITE_OUTCOME_UNKNOWN

Write tools never retry. When the response of a write is lost after the
request reached Gogs, the tool reports `WRITE_OUTCOME_UNKNOWN`. Look up the
result instead of repeating the write: use `get_issue` or
`list_issue_comments` to check whether the issue or comment exists, and only
repeat the write when it did not land. Repeating a write that did land
creates a duplicate.

## Logs pollute the MCP protocol

The server reserves stdout for MCP protocol messages and writes all logs to
stderr as JSON. If a wrapper script redirects stdout, Claude Code sees
protocol errors; run the binary directly as installed.

## The snapshot cache grows large

The code search cache stores extracted source snapshots under the user
cache directory, bounded by `GOGS_MCP_CACHE_MAX_BYTES` (2 GiB by default)
and `GOGS_MCP_CACHE_TTL` (24 hours by default). Remove the current user's
cache explicitly with:

```bash
~/.local/bin/gogs-mcp cache clean
```
