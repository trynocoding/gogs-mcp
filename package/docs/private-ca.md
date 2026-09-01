# Private CA and proxy configuration

Internal Gogs instances frequently terminate TLS with a certificate issued
by a company CA, and access from corporate networks frequently goes through
an HTTP proxy. The server supports both with environment variables.

## Trust a private CA

Point `GOGS_CA_FILE` at a PEM file that contains the certificate chain of
your company CA:

```bash
claude mcp add gogs \
  --scope user \
  --transport stdio \
  --env GOGS_BASE_URL="https://gogs.internal.example/" \
  --env GOGS_TOKEN_FILE="$HOME/.config/gogs-mcp/token" \
  --env GOGS_CA_FILE="$HOME/.config/gogs-mcp/company-ca.pem" \
  -- "$HOME/.local/bin/gogs-mcp" serve
```

Export the CA certificate from your IT-managed trust store or the browser
and store it as a read-only file (mode 0644 is sufficient; it is a public
certificate, not a secret). The file must contain PEM-encoded certificates.
The server adds the certificates to the system trust store for its own
connections only and never modifies the system configuration.

Alternatively, install the company CA into the Fedora system trust store:

```bash
sudo cp company-ca.pem /etc/pki/ca-trust/source/anchors/
sudo update-ca-trust
```

With the CA in the system trust store, `GOGS_CA_FILE` is not needed.

## Use a proxy

The standard environment variables `HTTPS_PROXY`, `HTTP_PROXY`, and
`NO_PROXY` are honored. Claude Code passes the environment of the parent
process to the server subprocess, so a proxy configured for your shell
applies to gogs-mcp as well. To set a proxy only for the server, add it to
the `claude mcp add` options:

```bash
claude mcp add gogs \
  --scope user \
  --transport stdio \
  --env HTTPS_PROXY="http://proxy.internal.example:3128" \
  --env NO_PROXY="gogs.internal.example" \
  ... (the GOGS_ variables from claude-code.md) \
  -- "$HOME/.local/bin/gogs-mcp" serve
```

Always exclude the Gogs host with `NO_PROXY` when the proxy cannot reach the
internal instance directly. Plain HTTP to Gogs is rejected unless
`GOGS_ALLOW_INSECURE_HTTP=true` is set explicitly, so an accidentally
downgraded scheme fails loudly instead of silently sending the token in
clear text.
