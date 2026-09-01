# Personal access token management

The server authenticates to Gogs with a personal access token (PAT) that is
stored in a private file. The token file is the only place the token lives;
it never appears in configuration files, shell history, or logs.

## Create a token

Generate the token in the Gogs web interface under your profile settings,
Applications, Generate new token. Grant only the scopes the server needs
for your work.

Store it in a private file:

```bash
mkdir -p -m 0700 ~/.config/gogs-mcp
install -m 0600 /dev/null ~/.config/gogs-mcp/token
printf '%s\n' 'YOUR_GOGS_PAT' > ~/.config/gogs-mcp/token
```

The server refuses to start when the token file is readable by anyone other
than its owner, so a file with permissions wider than 0600 fails with an
explicit error.

## Rotate a token

1. Generate a new token in the Gogs web interface.
2. Write the new token into the token file (the file keeps its 0600
   permissions).
3. Restart the MCP session in Claude Code so the server reads the new
   token.
4. Revoke the old token in the Gogs web interface.

## Revoke a token

Delete the token in the Gogs web interface; Gogs rejects it immediately.
Then remove or replace the local token file, because a revoked token kept on
disk has no value and only invites confusion.

## Keep the token private

- Never paste the token into shell commands that are recorded in history;
  use the file and `GOGS_TOKEN_FILE` instead.
- Never commit the token file or its contents to any repository.
- The server redacts the token from its logs, and the test suites of the
  product assert that logs never contain it.
- The `verify` command prints machine-readable results without the token.
