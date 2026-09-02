# Deploying over Streamable HTTP

The stdio transport starts one server process per user and requires every
user to install the binary and configure a token locally. The Streamable
HTTP transport serves everyone from one central process: users only
configure a URL and their own Gogs personal access token, and the server
holds no credential of its own.

## Start the server

The HTTP transport requires `GOGS_MCP_TRANSPORT=http` and a listen
address. A fixed token is rejected: the HTTP transport authenticates every
request with the caller's own credential instead.

```bash
GOGS_BASE_URL="https://gogs.internal.example/" \
GOGS_MCP_TRANSPORT=http \
GOGS_MCP_HTTP_ADDR="127.0.0.1:8080" \
gogs-mcp serve
```

Every request must carry a Gogs personal access token in the
`Authorization` header (`Bearer <token>`, `token <token>`, or the bare
token). The header name can be changed with
`GOGS_MCP_HTTP_TOKEN_HEADER`, which is useful when a proxy already owns
`Authorization`. The server probes Gogs once per token, keeps the resolved
user for `GOGS_MCP_HTTP_USER_CACHE_TTL` (5 minutes by default, at most
1 hour), and answers an invalid token with `401` without probing Gogs
again until the negative cache expires. Requests that fail for other
reasons receive `503` with `Retry-After: 5` and are never cached.

The transport is stateless: every POST is independent and no session
state outlives a request, so any load balancer may distribute the
traffic. `GET /healthz` answers `200` without credentials for load
balancer probes.

## Register the endpoint

```bash
claude mcp add gogs \
  --transport http \
  --header "Authorization: token $GOGS_PAT" \
  https://mcp.internal.example/mcp
```

Each user registers their own personal access token; nothing is shared
between users except the process and its caches.

## Terminate TLS with a reverse proxy

The server speaks plain HTTP and warns when it listens on a
non-loopback address. Put a reverse proxy in front of it for TLS,
authentication, and rate limiting.

nginx needs response buffering disabled so server-sent events stream
instead of stalling:

```nginx
location /mcp {
    proxy_pass http://127.0.0.1:8080;
    proxy_http_version 1.1;
    proxy_set_header Host $host;
    proxy_set_header Connection "";
    proxy_buffering off;
    proxy_read_timeout 1h;
}

location /healthz {
    proxy_pass http://127.0.0.1:8080;
}
```

Caddy streams by default:

```caddy
mcp.internal.example {
    reverse_proxy 127.0.0.1:8080
}
```

Do not strip or rewrite the credential header before the server sees it,
and do not cache any response of the MCP endpoint.

## Security checklist

- Terminate TLS at the proxy and keep the proxy-to-server hop on a
  loopback or private network; the credential header is plaintext there.
- Expose the server only to the intended audience. Every holder of a
  valid Gogs token can read everything that token can read in Gogs.
- Keep `GOGS_MCP_HTTP_ADDR` on a loopback address unless the proxy runs
  on the same host; the server logs a warning otherwise.
- Logs never contain tokens: the access log does not read the credential
  header and every per-user log line is redacted. Keep it that way when
  forwarding logs onward.
- A revoked token stays accepted for up to the user cache TTL. Set
  `GOGS_MCP_HTTP_USER_CACHE_TTL` lower when revocation latency matters;
  a TTL of `30s` bounds the exposure to half a minute.
- The per-user tool server honors `GOGS_MCP_WRITE_ENABLED` for every
  user. Leave it unset unless issue writing is intended for everyone
  connecting.

## Capacity and maintenance

Disk use grows with the number of active users: every user gets an
isolated snapshot and pull request cache subtree under
`GOGS_MCP_CACHE_DIR/users/<userID>`, each bounded by
`GOGS_MCP_CACHE_MAX_BYTES` (2 GiB by default) and `GOGS_MCP_CACHE_TTL`
(24 hours). A busy deployment with hundreds of users should lower the
per-user bound or provision disk accordingly.

The resolved-user cache keeps at most `GOGS_MCP_HTTP_MAX_USERS` users in
memory (128 by default) and evicts the least recently used ones;
eviction only drops the in-memory client, not the on-disk cache.

Reclaim disk with:

```bash
gogs-mcp cache clean --user 42    # one user, no credentials needed
gogs-mcp cache clean --all       # everything, no credentials needed
```

`verify` and the token-based `cache clean` still work against an HTTP
deployment: give that single command `GOGS_TOKEN` or `GOGS_TOKEN_FILE`
in addition to the deployment configuration.
