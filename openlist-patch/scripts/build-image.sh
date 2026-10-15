#!/usr/bin/env bash
# Build the patched OpenList image from the pinned inputs.
#
#   scripts/build-image.sh            → localhost/openlist-connect:<ver>-connect.<n> (+ :latest)
#   EXAMPLE=1 scripts/build-image.sh  → …-example tags, with the example adapter
#                                       registered (end-to-end tests only)
#
# Environment: OPENLIST_MIRROR / FRONTEND_TARBALL (see prepare-tree.sh),
# IMAGE (repository name, default localhost/openlist-connect).
set -euo pipefail

here="$(cd "$(dirname "$0")/.." && pwd)"
# shellcheck source=../UPSTREAM
. "$here/UPSTREAM"

repo="${IMAGE:-localhost/openlist-connect}"
tags=jsoniter
suffix=""
if [ "${EXAMPLE:-0}" = 1 ]; then
  tags="jsoniter loginsdk_example"
  suffix="-example"
fi
tag="$OPENLIST_VERSION-connect.$CONNECT_VERSION$suffix"

work="${WORK_DIR:-${XDG_CACHE_HOME:-$HOME/.cache}/openlist-connect/work}"
mkdir -p "$work"
tree="$(mktemp -d "$work/image.XXXXXX")"
trap 'rm -rf "$tree"' EXIT
"$here/scripts/prepare-tree.sh" "$tree/openlist"
# shellcheck source=/dev/null
. "$tree/openlist/CONNECT_BUILD.env"

cd "$HOME"
podman build \
  -f "$here/Containerfile" \
  --build-arg GO_IMAGE="$GO_IMAGE" \
  --build-arg RUNTIME_IMAGE="$RUNTIME_IMAGE" \
  --build-arg GO_TAGS="$tags" \
  --build-arg BUILT_AT="$(date -u +'%F %T %z')" \
  --build-arg OPENLIST_VERSION="$OPENLIST_VERSION" \
  --build-arg OPENLIST_COMMIT="$OPENLIST_COMMIT" \
  --build-arg OPENLIST_COMMIT_SHORT="${OPENLIST_COMMIT:0:7}" \
  --build-arg FRONTEND_VERSION="$FRONTEND_VERSION" \
  --build-arg CONNECT_VERSION="$CONNECT_VERSION" \
  --build-arg PATCH_SOURCE_REVISION="$PATCH_SOURCE_REVISION" \
  -t "$repo:$tag" -t "$repo:latest$suffix" \
  "$tree/openlist"
echo "built $repo:$tag (upstream $OPENLIST_VERSION $OPENLIST_COMMIT, connect $CONNECT_VERSION, source $PATCH_SOURCE_REVISION, tags: $tags)"
