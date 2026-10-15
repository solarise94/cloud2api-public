// Module boundary only. The Go code under src/ belongs to the OpenList module
// (github.com/OpenListTeam/OpenList/v4) and is compiled inside a tree
// assembled by scripts/prepare-tree.sh; this file keeps cloud2api's
// `go build/vet/test ./...` from walking into it.
module cloud2api/openlist-patch

go 1.25
