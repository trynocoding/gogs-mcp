# Installing gogs-mcp offline

This document describes how to install, upgrade, and remove the gogs-mcp
offline bundle on a Fedora 43 Linux AMD64 machine that has no access to the
public internet.

## What is in the bundle

- `bin/gogs-mcp` is a statically linked Linux AMD64 binary.
- `source/gogs-mcp-<version>.tar.gz` contains the full source tree with the
  vendored Go dependencies, so the bundle can be audited and rebuilt with a
  local Go toolchain.
- `images/gogs-mcp-<version>-linux-amd64-oci.tar` is the OCI image of the
  server; see `container.md` for importing and running it with Podman or
  Docker.
- `test-assets/gogs-0.14.2-amd64-image.tar` holds the pinned Gogs v0.14.2
  E2E image for offline test replays; bundles assembled without a container
  engine or the pinned Gogs checkout ship without it.
- `scripts/` contains the install, uninstall, offline verification, and
  image loading scripts.
- `docs/` contains the product documentation.
- `config/` contains example configuration files.
- `MANIFEST.sha256` lists the SHA-256 digest of every package file.
- `SBOM.cdx.json`, `THIRD_PARTY_LICENSES.txt`, and `SOURCE-METADATA.json`
  describe the software supply chain of the bundle.

## Prerequisites

The verification and installation scripts use `sha256sum`, `tar`, and `file`
from the Fedora base installation. Rebuilding the bundled source needs a Go
1.25 toolchain or newer; the binary itself needs nothing else.

## Verify the package

After extracting the archive, run the offline verification. It checks the
manifest checksums, the binary architecture and static linking, the version
identity of the binary and the source archive, the digest of the bundled
Gogs E2E image when one is present, and the stdio MCP protocol.
None of these steps access the network.

```bash
tar -xzf gogs-mcp-<version>-fedora43-amd64-offline.tar.gz
cd gogs-mcp-<version>-fedora43-amd64-offline
scripts/verify-offline.sh
```

## Install

```bash
scripts/install.sh
```

The script installs the binary into `~/.local/bin/gogs-mcp`, the
documentation and examples into `~/.local/share/doc/gogs-mcp/`, and records
every installed file in `~/.local/share/gogs-mcp/installed-files.list`. Pass
`--prefix DIR` to install below another directory. The script does not
create a personal access token and does not write any configuration.

## Configure the token

Create a personal access token file with owner-only permissions as described
in `pat-management.md`:

```bash
mkdir -p -m 0700 ~/.config/gogs-mcp
install -m 0600 /dev/null ~/.config/gogs-mcp/token
printf '%s\n' 'YOUR_GOGS_PAT' > ~/.config/gogs-mcp/token
```

Replace `YOUR_GOGS_PAT` with a token generated in the Gogs web interface.

## Configure Claude Code

Register the installed binary as a user-scoped stdio MCP server as described
in `claude-code.md`.

## Verify the connection

```bash
GOGS_BASE_URL="https://gogs.internal.example/" \
GOGS_TOKEN_FILE="$HOME/.config/gogs-mcp/token" \
~/.local/bin/gogs-mcp verify
```

Replace the base URL with the address of your internal Gogs instance. The
command resolves the authenticated user over the network to Gogs and prints
a machine-readable result without the token.

## Upgrade

Run the install script of the new bundle with the same prefix. The script
overwrites the binary and the documentation and refreshes the list of
installed files. Configuration, tokens, and the snapshot cache are never
touched by an upgrade.

## Uninstall

```bash
scripts/uninstall.sh
```

The script removes exactly the files recorded in the installation manifest
and no others. Your configuration, personal access token, and snapshot cache
are kept. Remove them explicitly with:

```bash
scripts/uninstall.sh --purge
```

`--purge` deletes the snapshot cache, the configuration directory
`~/.config/gogs-mcp/` including the token file, and then the installation
itself.
