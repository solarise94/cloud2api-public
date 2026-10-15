#!/usr/bin/env bash
# Assemble the patched OpenList source tree from pinned inputs (UPSTREAM):
#   1. upstream OpenList at OPENLIST_COMMIT (the tag must resolve to it),
#   2. patches/*.patch applied in order (the only edits to upstream files),
#   3. src/ copied in as NEW files only (refuses to overwrite upstream code),
#   4. the pinned upstream web UI unpacked into public/dist (sha256-checked).
#
# Usage: prepare-tree.sh <dest-dir>
#
# Environment:
#   OPENLIST_MIRROR   local git clone of OpenList to clone from instead of
#                     OPENLIST_REPO (the commit must exist in it)
#   FRONTEND_TARBALL  local copy of the frontend tarball (still sha256-checked)
#   SKIP_FRONTEND=1   leave public/dist as upstream ships it (unit tests only;
#                     a binary built from such a tree cannot serve the web UI)
#   CACHE_DIR         download cache (default ~/.cache/openlist-connect)
set -euo pipefail

here="$(cd "$(dirname "$0")/.." && pwd)"
# shellcheck source=../UPSTREAM
. "$here/UPSTREAM"

dest="${1:?usage: prepare-tree.sh <dest-dir>}"
if [ -e "$dest" ] && [ -n "$(ls -A "$dest" 2>/dev/null)" ]; then
  echo "prepare-tree: $dest exists and is not empty" >&2
  exit 1
fi
cache="${CACHE_DIR:-${XDG_CACHE_HOME:-$HOME/.cache}/openlist-connect}"
mkdir -p "$cache"

echo "==> upstream OpenList $OPENLIST_VERSION ($OPENLIST_COMMIT)"
if [ -n "${OPENLIST_MIRROR:-}" ]; then
  git clone -q --no-checkout "$OPENLIST_MIRROR" "$dest"
else
  git clone -q --no-checkout --filter=blob:none "$OPENLIST_REPO" "$dest"
fi
git -C "$dest" -c advice.detachedHead=false checkout -q "$OPENLIST_COMMIT"
head="$(git -C "$dest" rev-parse HEAD)"
if [ "$head" != "$OPENLIST_COMMIT" ]; then
  echo "prepare-tree: HEAD is $head, expected $OPENLIST_COMMIT" >&2
  exit 1
fi
if tagged="$(git -C "$dest" rev-parse -q --verify "refs/tags/$OPENLIST_VERSION^{commit}" 2>/dev/null)"; then
  if [ "$tagged" != "$OPENLIST_COMMIT" ]; then
    echo "prepare-tree: tag $OPENLIST_VERSION is $tagged, pinned commit is $OPENLIST_COMMIT" >&2
    exit 1
  fi
else
  echo "    (tag $OPENLIST_VERSION not present in the clone; pinned by commit only)"
fi

echo "==> patches"
for p in "$here"/patches/*.patch; do
  git -C "$dest" apply --check "$p"
  git -C "$dest" apply "$p"
  echo "    applied $(basename "$p")"
done

echo "==> connect module (src/ as new files)"
(cd "$here/src" && find . -type f | sort) | while read -r rel; do
  rel="${rel#./}"
  if [ -e "$dest/$rel" ]; then
    echo "prepare-tree: src/$rel would overwrite an upstream file; upstream edits belong in patches/" >&2
    exit 1
  fi
  mkdir -p "$dest/$(dirname "$rel")"
  cp "$here/src/$rel" "$dest/$rel"
done
echo "    copied $(cd "$here/src" && find . -type f | wc -l) files"

if [ "${SKIP_FRONTEND:-0}" = 1 ]; then
  echo "==> frontend skipped (SKIP_FRONTEND=1)"
else
  echo "==> upstream web UI $FRONTEND_VERSION"
  tarball="${FRONTEND_TARBALL:-$cache/openlist-frontend-dist-$FRONTEND_VERSION.tar.gz}"
  if [ ! -f "$tarball" ]; then
    curl -fsSL --retry 3 -o "$tarball.part" "$FRONTEND_URL"
    mv "$tarball.part" "$tarball"
  fi
  sum="$(sha256sum "$tarball" | awk '{print $1}')"
  if [ "$sum" != "$FRONTEND_SHA256" ]; then
    echo "prepare-tree: frontend sha256 $sum, expected $FRONTEND_SHA256" >&2
    exit 1
  fi
  rm -rf "$dest/public/dist"
  mkdir -p "$dest/public/dist"
  tar -xzf "$tarball" -C "$dest/public/dist"
  [ -f "$dest/public/dist/index.html" ] || { echo "prepare-tree: frontend tarball has no index.html" >&2; exit 1; }
fi

src_rev="$(git -C "$here" rev-parse --short HEAD 2>/dev/null || echo unknown)"
if [ -n "$(git -C "$here" status --porcelain -- . 2>/dev/null)" ]; then
  src_rev="$src_rev-dirty"
fi
cat > "$dest/CONNECT_BUILD.env" <<EOF
OPENLIST_VERSION=$OPENLIST_VERSION
OPENLIST_COMMIT=$OPENLIST_COMMIT
FRONTEND_VERSION=$FRONTEND_VERSION
CONNECT_VERSION=$CONNECT_VERSION
PATCH_SOURCE_REVISION=$src_rev
EOF
echo "==> tree ready: $dest (connect $CONNECT_VERSION, patch source $src_rev)"
