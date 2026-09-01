#!/bin/sh
# uninstall.sh removes exactly the files that install.sh recorded, and never
# touches files outside that product manifest. User data (configuration,
# personal access tokens, and the snapshot cache) is kept unless --purge is
# given explicitly.
#
# Usage: scripts/uninstall.sh [--prefix DIR] [--purge]
set -eu

usage() {
	echo "Usage: scripts/uninstall.sh [--prefix DIR] [--purge]" >&2
	echo "  --prefix DIR  Uninstall from DIR (default: \$HOME/.local)." >&2
	echo "  --purge       Also remove the configuration, the personal access" >&2
	echo "                token file, and the snapshot cache." >&2
}

prefix=""
purge=0
while [ $# -gt 0 ]; do
	case "$1" in
	--prefix)
		[ $# -ge 2 ] || { echo "uninstall.sh: --prefix needs a directory." >&2; exit 2; }
		prefix="$2"
		shift 2
		;;
	--prefix=*)
		prefix="${1#*=}"
		shift
		;;
	--purge)
		purge=1
		shift
		;;
	-h | --help)
		usage
		exit 0
		;;
	*)
		echo "uninstall.sh: unknown option $1." >&2
		usage
		exit 2
		;;
	esac
done

if [ -z "$prefix" ]; then
	if [ -z "${HOME:-}" ]; then
		echo "uninstall.sh: HOME is not set and no --prefix was given." >&2
		exit 2
	fi
	prefix="$HOME/.local"
fi

installed_list="$prefix/share/gogs-mcp/installed-files.list"
if [ ! -f "$installed_list" ]; then
	echo "uninstall.sh: no gogs-mcp installation found at $prefix." >&2
	exit 1
fi

if [ "$purge" -eq 1 ]; then
	echo "Purging the snapshot cache and the user configuration."
	if [ -x "$prefix/bin/gogs-mcp" ]; then
		# cache clean --all only deletes snapshot cache roots; the placeholder
		# configuration satisfies the config check and never reaches the network.
		GOGS_BASE_URL="https://gogs.invalid/" GOGS_TOKEN="uninstall-purge" \
			"$prefix/bin/gogs-mcp" cache clean --all || true
	elif [ -n "${HOME:-}" ]; then
		rm -rf "$HOME/.cache/gogs-mcp"
	fi
	if [ -n "${HOME:-}" ]; then
		rm -rf "$HOME/.config/gogs-mcp"
		# cache clean --all keeps the cache root directory itself; remove the
		# now-empty root so a purged machine has no leftovers.
		rmdir "$HOME/.cache/gogs-mcp" 2>/dev/null || true
	fi
fi

echo "Removing the recorded installation files."
# Delete files deepest first so the following directory pass can remove
# every directory that this installation created and nothing else.
sort -r "$installed_list" | while IFS= read -r file; do
	if [ -f "$file" ] || [ -L "$file" ]; then
		rm -f "$file"
	fi
done

sort -r "$installed_list" | while IFS= read -r file; do
	directory=$(dirname "$file")
	while [ "$directory" != "$prefix" ] && [ "$directory" != "/" ]; do
		rmdir "$directory" 2>/dev/null || break
		directory=$(dirname "$directory")
	done
done

rm -f "$installed_list"
rmdir "$prefix/share/gogs-mcp" 2>/dev/null || true
rmdir "$prefix/share" 2>/dev/null || true

if [ "$purge" -eq 1 ]; then
	echo "Removed gogs-mcp from $prefix and purged its user data."
else
	echo "Removed gogs-mcp from $prefix. Configuration, tokens, and the snapshot"
	echo "cache were kept; pass --purge to remove them as well."
fi
