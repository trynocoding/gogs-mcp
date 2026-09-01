# Running gogs-mcp as a container

The offline bundle ships a Linux AMD64 OCI image of the server in
`images/gogs-mcp-<version>-linux-amd64-oci.tar`. The image contains only the
statically linked binary and an empty cache directory: it has no shell, no
package manager, and no compiler, it runs as the non-root user 65532:65532,
and its root file system can be mounted read-only. Podman and Docker both
run it over stdin/stdout, which is how Claude Code talks to an MCP server.

## Import the image

Use the load script of the bundle. It imports the local archive with pulls
disabled and verifies the architecture, the container user, and the product
version and commit against `SOURCE-METADATA.json`:

```bash
scripts/load-image.sh
```

Pass `--engine docker` or `--engine podman` to pick an engine explicitly,
and `--image NAME:TAG` to load the image under another name. The script
prints the verified reference, `gogs-mcp:<version>` by default.

## Run the server

The container reads its configuration from environment variables, and the
personal access token and the optional private CA certificate come from
read-only file mounts. Nothing ever pulls from a registry: every command
uses `--pull=never`.

Docker:

```bash
docker run --rm --interactive --pull=never --read-only \
  --user "$(id -u):$(id -g)" \
  --volume gogs-mcp-cache:/cache \
  --volume "$HOME/.config/gogs-mcp/token:/run/secrets/gogs-token:ro" \
  --env GOGS_BASE_URL="https://gogs.internal.example/" \
  --env GOGS_TOKEN_FILE=/run/secrets/gogs-token \
  gogs-mcp:<version> serve
```

Podman:

```bash
podman run --rm --interactive --pull=never --read-only \
  --userns=keep-id \
  --volume gogs-mcp-cache:/cache \
  --volume "$HOME/.config/gogs-mcp/token:/run/secrets/gogs-token:ro,Z" \
  --env GOGS_BASE_URL="https://gogs.internal.example/" \
  --env GOGS_TOKEN_FILE=/run/secrets/gogs-token \
  gogs-mcp:<version> serve
```

The image user is 65532:65532, and a container started without `--user` or
`--userns` runs as that user. The examples remap the container user to the
account that owns the mounted token file, because the token file must keep
its 0600 permissions and can therefore only be read by its owner. The cache
directory of the image is world-writable, so the named cache volume stays
usable under either identity. On Fedora, Docker usually needs no extra
flags, while rootless Podman binds need the `,Z` relabeling shown above.

`config/docker-mcp.example.json` and `config/podman-mcp.example.json` in
the bundle show the same commands as Claude Code MCP server definitions;
they contain file paths only, never a token value. The Docker JSON example
keeps the image user 65532:65532, so the token file it mounts
(`container-token`) must be owned by UID 65532, which a root-managed
machine can arrange with `chown 65532:65532`; the Podman JSON example uses
`--userns=keep-id` and reads the regular token file of the account.

## Register with Claude Code

Register the container as a user-scoped stdio server, for example with
Podman:

```bash
claude mcp add \
  --scope user \
  --transport stdio \
  gogs \
  -- podman run --rm --interactive --pull=never --read-only \
    --userns=keep-id \
    --volume gogs-mcp-cache:/cache \
    --volume "$HOME/.config/gogs-mcp/token:/run/secrets/gogs-token:ro,Z" \
    --env GOGS_BASE_URL="https://gogs.internal.example/" \
    --env GOGS_TOKEN_FILE=/run/secrets/gogs-token \
    gogs-mcp:<version> serve
```

The `--interactive` flag keeps the protocol on stdin and stdout, `--rm`
removes the container after the session, and `--pull=never` makes a missing
local image fail loudly instead of reaching for a registry. Container
stdout carries only MCP protocol messages; the server writes its logs to
stderr, which Claude Code keeps out of the protocol stream.

Check the registration with `claude mcp get gogs` or `/mcp` inside Claude
Code.

## Cache and upgrades

The named volume `gogs-mcp-cache` holds the snapshot cache of the code
search tools across container restarts; the container root file system
stays read-only. To start from an empty cache, remove the volume with
`docker volume rm gogs-mcp-cache` or `podman volume rm gogs-mcp-cache`.

To upgrade, run `scripts/load-image.sh` of the new bundle: it imports the
new image under its version tag and verifies its identity. Containers
started with `--rm` pick up the new tag after the Claude Code registration
references it.

To remove the image again, delete the loaded tag with
`docker rmi gogs-mcp:<version>` or `podman rmi gogs-mcp:<version>`.
