# Using gogs-mcp with Claude Code

Claude Code starts gogs-mcp as a local stdio MCP server subprocess. The
recommended configuration is user-scoped, so the server is available in
every project.

## Register the server

```bash
claude mcp add \
  --scope user \
  --transport stdio \
  --env GOGS_BASE_URL="https://gogs.internal.example/" \
  --env GOGS_TOKEN_FILE="$HOME/.config/gogs-mcp/token" \
  gogs \
  -- "$HOME/.local/bin/gogs-mcp" serve
```

Replace the base URL with the address of your internal Gogs instance. The
example never contains a real address or a token; the token stays in the
private file referenced by `GOGS_TOKEN_FILE`.

`config/mcp.example.json` in the bundle shows the same registration as a
JSON fragment for environments where MCP servers are configured in a
settings file.

Check the registration with `claude mcp get gogs` or `/mcp` inside Claude
Code.

## Enable the write tools

By default the server only registers read-only tools. Set
`GOGS_MCP_WRITE_ENABLED=true` in the `--env` options above to also register
`create_issue`, `update_issue`, and `create_issue_comment`. Writes are never
retried automatically: when the outcome of a write is unknown, the server
reports `WRITE_OUTCOME_UNKNOWN` and the result must be looked up instead of
repeating the write.

## Scope of this server

Gogs issues track repository-internal discussion. Jira remains the
requirements system of record, and gogs-mcp does not execute Jira or
Jenkins orchestration; orchestration tools are separate MCP servers that
Claude Code calls independently.
