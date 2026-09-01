#!/bin/sh
# verify-offline.sh checks an extracted gogs-mcp offline bundle without any
# network access: it verifies the SHA-256 manifest, confirms the binary
# architecture and static linking, ties the binary to its source archive
# through SOURCE-METADATA.json, and exercises the stdio MCP protocol with a
# placeholder configuration that never reaches a server.
#
# Usage: scripts/verify-offline.sh [PACKAGE_DIR]
set -eu

package_dir="${1:-}"
if [ -z "$package_dir" ]; then
	package_dir=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
fi
case "$package_dir" in
/*) ;;
*) package_dir=$(CDPATH= cd -- "$package_dir" && pwd) ;;
esac
cd "$package_dir"

for required in bin/gogs-mcp MANIFEST.sha256 SOURCE-METADATA.json; do
	[ -f "$required" ] || {
		echo "verify-offline.sh: $package_dir does not look like a gogs-mcp package (missing $required)." >&2
		exit 1
	}
done

echo "Verifying the package checksums."
sha256sum -c MANIFEST.sha256 --quiet

echo "Checking the binary architecture and linking."
binary_kind=$(file bin/gogs-mcp)
case "$binary_kind" in
*x86-64*) ;;
*)
	echo "verify-offline.sh: the binary is not an x86-64 build: $binary_kind." >&2
	exit 1
	;;
esac
case "$binary_kind" in
*statically*) ;;
*)
	echo "verify-offline.sh: the binary is not statically linked: $binary_kind." >&2
	exit 1
	;;
esac

# json_value FILE KEY reads the string value of "key": "..." from the
# machine-readable files in this package. The binary prints its version JSON
# on one line, SOURCE-METADATA.json uses two-space indentation.
json_value() {
	file="$1"
	key="$2"
	sed -n "s/.*\"$key\"[[:space:]]*:[[:space:]]*\"\([^\"]*\)\".*/\1/p" "$file" | head -n 1
}

echo "Comparing the binary version with SOURCE-METADATA.json."
version_json=$(bin/gogs-mcp version --json)
metadata=SOURCE-METADATA.json
for key in version commit build_time; do
	binary_value=$(printf '%s\n' "$version_json" | json_value /dev/stdin "$key")
	metadata_value=$(json_value "$metadata" "$key")
	if [ -z "$binary_value" ] || [ "$binary_value" != "$metadata_value" ]; then
		echo "verify-offline.sh: the binary $key ($binary_value) does not match SOURCE-METADATA.json ($metadata_value)." >&2
		exit 1
	fi
done

echo "Checking the source archive identity."
# Scope the reads to the source_archive block so the binary artifact's path
# and digest cannot be mistaken for the source archive's.
source_block=$(sed -n '/"source_archive"/,/}/p' "$metadata")
source_file=$(printf '%s\n' "$source_block" | sed -n 's/.*"path": "\([^"]*\)".*/\1/p')
source_digest=$(printf '%s\n' "$source_block" | sed -n 's/.*"sha256": "\([^"]*\)".*/\1/p')
if [ -z "$source_file" ] || [ ! -f "$source_file" ]; then
	echo "verify-offline.sh: SOURCE-METADATA.json points at the missing source archive $source_file." >&2
	exit 1
fi
actual_digest=$(sha256sum "$source_file")
actual_digest=${actual_digest%% *}
if [ "$actual_digest" != "$source_digest" ]; then
	echo "verify-offline.sh: the source archive digest does not match SOURCE-METADATA.json." >&2
	exit 1
fi

# The Gogs E2E image is optional: bundles assembled without docker or the
# pinned Gogs checkout ship without a test-assets directory.
if grep -q '"test_assets"' "$metadata"; then
	echo "Checking the bundled Gogs E2E image."
	test_block=$(sed -n '/"test_assets"/,/}/p' "$metadata")
	test_file=$(printf '%s\n' "$test_block" | sed -n 's/.*"path": "\([^"]*\)".*/\1/p')
	test_digest=$(printf '%s\n' "$test_block" | sed -n 's/.*"sha256": "\([^"]*\)".*/\1/p')
	if [ -z "$test_file" ] || [ ! -f "$test_file" ]; then
		echo "verify-offline.sh: SOURCE-METADATA.json points at the missing test asset $test_file." >&2
		exit 1
	fi
	actual_test_digest=$(sha256sum "$test_file")
	actual_test_digest=${actual_test_digest%% *}
	if [ "$actual_test_digest" != "$test_digest" ]; then
		echo "verify-offline.sh: the Gogs E2E image digest does not match SOURCE-METADATA.json." >&2
		exit 1
	fi
fi

echo "Exercising the stdio MCP protocol with a placeholder configuration."
initialize='{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"verify-offline","version":"0"}}}'
initialized='{"jsonrpc":"2.0","method":"notifications/initialized"}'
list_tools='{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}'
# Keep stdin open for a moment so the server processes the handshake before
# the end of input shuts the session down; nothing leaves the machine.
(printf '%s\n' "$initialize" "$initialized" "$list_tools"; sleep 5) |
	env GOGS_BASE_URL="https://gogs.invalid/" GOGS_TOKEN="offline-verification" \
		bin/gogs-mcp serve >.verify-responses.jsonl 2>/dev/null || true

responses=.verify-responses.jsonl
grep -q '"serverInfo"' "$responses" || {
	echo "verify-offline.sh: the server did not answer the MCP initialize request." >&2
	rm -f "$responses"
	exit 1
}
grep -q '"get_authenticated_user"' "$responses" || {
	echo "verify-offline.sh: the server did not advertise its tools over stdio." >&2
	rm -f "$responses"
	exit 1
}
rm -f "$responses"

echo "Offline verification passed: checksums, architecture, versions, source"
echo "identity, and the stdio MCP protocol all check out."
