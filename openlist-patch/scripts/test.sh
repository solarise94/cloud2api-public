#!/usr/bin/env bash
# Build and test the connect module inside a freshly prepared patched tree.
#
#   scripts/test.sh                 gofmt + vet + build + tests (+ -race)
#   scripts/test.sh -run TestFoo    extra arguments go to `go test`
#
# Environment:
#   OPENLIST_MIRROR   local OpenList clone (faster than cloning GitHub)
#   NO_RACE=1         skip the race-detector pass
#   KEEP_TREE=1       keep the prepared tree (path is printed)
#   PKGS              packages to test (default: every connect package)
set -euo pipefail

here="$(cd "$(dirname "$0")/.." && pwd)"
# shellcheck source=../UPSTREAM
. "$here/UPSTREAM"

work="${WORK_DIR:-${XDG_CACHE_HOME:-$HOME/.cache}/openlist-connect/work}"
mkdir -p "$work"
tree="$(mktemp -d "$work/tree.XXXXXX")"
cleanup() { if [ "${KEEP_TREE:-0}" != 1 ]; then rm -rf "$tree"; else echo "tree kept: $tree"; fi; }
trap cleanup EXIT

SKIP_FRONTEND=1 "$here/scripts/prepare-tree.sh" "$tree/openlist" >/dev/null

img=localhost/openlist-connect-gotest:latest
if ! podman image exists "$img"; then
  printf 'FROM %s\nRUN apk add --no-cache gcc musl-dev git\n' "$GO_IMAGE" | podman build -q -t "$img" -f - . >/dev/null
fi

pkgs="${PKGS:-./pkg/goapiclient/... ./pkg/loginsdk/... ./internal/loginbridge/... ./server/connect/... ./server/goapihop/... ./drivers/goapi/... ./internal/op}"
dirs="pkg/goapiclient pkg/loginsdk internal/loginbridge server/connect server/goapihop drivers/goapi internal/op"
race=1
[ "${NO_RACE:-0}" = 1 ] && race=0

cd "$HOME"
podman run --rm \
  -v "$tree/openlist:/src:Z" \
  -v openlist-gomod:/go/pkg/mod \
  -v openlist-gocache:/root/.cache/go-build \
  -w /src -e GOTOOLCHAIN=local -e GOFLAGS=-buildvcs=false \
  -e PKGS="$pkgs" -e DIRS="$dirs" -e RACE="$race" \
  "$img" sh -ec '
    for d in $DIRS; do [ -d "$d" ] || continue; out=$(gofmt -l "$d"); if [ -n "$out" ]; then echo "gofmt needed:"; echo "$out"; exit 1; fi; done
    echo "--- go vet"
    go vet -tags "jsoniter loginsdk_example" $PKGS
    echo "--- go build (whole program, production tags)"
    CGO_ENABLED=0 go build -tags jsoniter -o /dev/null .
    echo "--- go test (production tags)"
    go test -count=1 -tags jsoniter $PKGS "$@"
    echo "--- go test (with the example adapter registered)"
    go test -count=1 -tags "jsoniter loginsdk_example" $PKGS "$@"
    if [ "$RACE" = 1 ]; then
      echo "--- go test -race"
      CGO_ENABLED=1 go test -count=1 -race -tags "jsoniter loginsdk_example" $PKGS "$@"
    fi
  ' sh "$@"
echo "connect tests passed (OpenList $OPENLIST_VERSION, connect $CONNECT_VERSION)"
