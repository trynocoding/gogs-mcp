#!/bin/sh
# install.sh installs the files of the gogs-mcp offline bundle below a user
# prefix. The script never accesses the network and never creates, reads, or
# writes a personal access token; configuring a token is a separate manual
# step documented in docs/install.md.
#
# Usage: scripts/install.sh [--prefix DIR]
set -eu

usage() {
	echo "Usage: scripts/install.sh [--prefix DIR]" >&2
	echo "  --prefix DIR  Install below DIR (default: \$HOME/.local)." >&2
}

prefix=""
while [ $# -gt 0 ]; do
	case "$1" in
	--prefix)
		[ $# -ge 2 ] || { echo "install.sh: --prefix needs a directory." >&2; exit 2; }
		prefix="$2"
		shift 2
		;;
	--prefix=*)
		prefix="${1#*=}"
		shift
		;;
	-h | --help)
		usage
		exit 0
		;;
	*)
		echo "install.sh: unknown option $1." >&2
		usage
		exit 2
		;;
	esac
done

if [ -z "$prefix" ]; then
	if [ -z "${HOME:-}" ]; then
		echo "install.sh: HOME is not set and no --prefix was given." >&2
		exit 2
	fi
	prefix="$HOME/.local"
fi
case "$prefix" in
/*) ;;
*)
	echo "install.sh: the prefix must be an absolute path." >&2
	exit 2
	;;
esac

package_dir=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
# Every later step resolves package paths relative to the package directory,
# regardless of the caller's working directory.
cd "$package_dir"

echo "Verifying the package checksums."
sha256sum -c MANIFEST.sha256 --quiet

install_file() {
	source="$1"
	relative="$2"
	mode="$3"
	target="$prefix/$relative"
	mkdir -p "$(dirname "$target")"
	install -m "$mode" "$package_dir/$source" "$target"
	echo "$target"
}

installed_files=$(mktemp)
trap 'rm -f "$installed_files"' EXIT

install_file bin/gogs-mcp bin/gogs-mcp 0755 >>"$installed_files"
for document in docs/*.md; do
	name=${document#docs/}
	install_file "docs/$name" "share/doc/gogs-mcp/docs/$name" 0644 >>"$installed_files"
done
for sample in config/*; do
	name=${sample#config/}
	install_file "config/$name" "share/doc/gogs-mcp/config/$name" 0644 >>"$installed_files"
done
for asset in SBOM.cdx.json THIRD_PARTY_LICENSES.txt SOURCE-METADATA.json MANIFEST.sha256; do
	install_file "$asset" "share/gogs-mcp/$asset" 0644 >>"$installed_files"
done
# The list must not record itself: uninstall reads it after the file pass,
# and a self-referencing entry would destroy it mid-uninstall.

sort -u "$installed_files" >"$installed_files.sorted"
mkdir -p "$prefix/share/gogs-mcp"
mv "$installed_files.sorted" "$prefix/share/gogs-mcp/installed-files.list"
chmod 0644 "$prefix/share/gogs-mcp/installed-files.list"

echo "Installed gogs-mcp into $prefix. Installed files are listed in"
echo "$prefix/share/gogs-mcp/installed-files.list."
echo "Next steps: create a personal access token file and configure Claude Code,"
echo "see share/doc/gogs-mcp/docs/install.md."
