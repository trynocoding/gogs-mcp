#!/bin/sh
# load-image.sh imports the container image of this bundle into a local
# container engine and verifies the imported image against SOURCE-METADATA.json.
# The import never touches a registry: the engine reads the local archive, and
# every check runs with pulls disabled.
#
# Usage: scripts/load-image.sh [--engine docker|podman] [--image NAME:TAG] [PACKAGE_DIR]
set -eu

usage() {
	echo "Usage: scripts/load-image.sh [--engine docker|podman] [--image NAME:TAG] [PACKAGE_DIR]" >&2
	echo "  --engine       Container engine to use (default: podman, else docker)." >&2
	echo "  --image NAME   Load and verify the image under NAME:TAG instead of" >&2
	echo "                 the bundled gogs-mcp:<version> reference." >&2
}

engine=""
image=""
while [ $# -gt 0 ]; do
	case "$1" in
	--engine)
		[ $# -ge 2 ] || { echo "load-image.sh: --engine needs a value." >&2; exit 2; }
		engine="$2"
		shift 2
		;;
	--image)
		[ $# -ge 2 ] || { echo "load-image.sh: --image needs a NAME:TAG." >&2; exit 2; }
		image="$2"
		shift 2
		;;
	-h | --help)
		usage
		exit 0
		;;
	-*)
		echo "load-image.sh: unknown option $1." >&2
		usage
		exit 2
		;;
	*)
		break
		;;
	esac
done

package_dir="${1:-}"
if [ -z "$package_dir" ]; then
	package_dir=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
fi
case "$package_dir" in
/*) ;;
*) package_dir=$(CDPATH= cd -- "$package_dir" && pwd) ;;
esac
cd "$package_dir"

for required in bin/gogs-mcp MANIFEST.sha256 SOURCE-METADATA.json images; do
	[ -e "$required" ] || {
		echo "load-image.sh: $package_dir does not look like a gogs-mcp package (missing $required)." >&2
		exit 1
	}
done

archive_files=$(ls images/gogs-mcp-*-linux-amd64-oci.tar 2>/dev/null || true)
archive_count=$(printf '%s\n' "$archive_files" | grep -c . || true)
if [ "$archive_count" -ne 1 ]; then
	echo "load-image.sh: expected exactly one OCI image archive under images/, found $archive_count." >&2
	exit 1
fi
archive="$archive_files"

if [ -z "$engine" ]; then
	if command -v podman >/dev/null 2>&1; then
		engine="podman"
	elif command -v docker >/dev/null 2>&1; then
		engine="docker"
	else
		echo "load-image.sh: no container engine found; install podman or docker." >&2
		exit 1
	fi
fi

# json_value FILE KEY reads the string value of "key": "..." from the
# machine-readable files in this package.
json_value() {
	file="$1"
	key="$2"
	sed -n "s/.*\"$key\"[[:space:]]*:[[:space:]]*\"\([^\"]*\)\".*/\1/p" "$file" | head -n 1
}

metadata=SOURCE-METADATA.json
metadata_version=$(json_value "$metadata" version)
metadata_commit=$(json_value "$metadata" commit)
if [ -z "$image" ]; then
	image="gogs-mcp:$metadata_version"
fi

echo "Importing $archive into $engine."
"$engine" load -i "$archive"

if ! "$engine" image inspect --format '{{.Os}}' "$image" >/dev/null 2>&1; then
	# Some engines record an imported OCI archive without a resolvable tag;
	# retag the loaded image by its ID before verifying it.
	image_id=$("$engine" images --format '{{.ID}} {{.Repository}}:{{.Tag}}' | while IFS= read -r entry; do
		case "$entry" in
		*" $image") printf '%s\n' "${entry%% *}" ;;
		esac
	done | head -n 1)
	if [ -z "$image_id" ]; then
		echo "load-image.sh: the imported image $image was not found after loading." >&2
		exit 1
	fi
	"$engine" tag "$image_id" "$image"
fi

echo "Checking the imported image platform and user."
inspected=$("$engine" image inspect --format '{{.Os}} {{.Architecture}} {{.Config.User}}' "$image")
if [ "$inspected" != "linux amd64 65532:65532" ]; then
	echo "load-image.sh: the imported image reports \"$inspected\" instead of \"linux amd64 65532:65532\"." >&2
	exit 1
fi

echo "Comparing the image identity with SOURCE-METADATA.json."
version_json=$("$engine" run --rm --pull=never "$image" version --json)
image_version=$(printf '%s\n' "$version_json" | json_value /dev/stdin version)
image_commit=$(printf '%s\n' "$version_json" | json_value /dev/stdin commit)
if [ -z "$image_version" ] || [ "$image_version" != "$metadata_version" ]; then
	echo "load-image.sh: the image version ($image_version) does not match SOURCE-METADATA.json ($metadata_version)." >&2
	exit 1
fi
if [ -z "$image_commit" ] || [ "$image_commit" != "$metadata_commit" ]; then
	echo "load-image.sh: the image commit ($image_commit) does not match SOURCE-METADATA.json ($metadata_commit)." >&2
	exit 1
fi

echo "Image verification passed: $image is linux/amd64, runs as 65532:65532,"
echo "and carries gogs-mcp $image_version ($image_commit)."
