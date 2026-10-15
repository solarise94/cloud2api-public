package goapi

// Reserved virtual system directories and their private backing aliases.
//
// A cloud2api peer refuses /.c2a-trash and /.relay as /v1 paths (both names
// are managed by dedicated peer APIs), while an OpenList gateway in front of
// this driver needs both at the top of this storage. The driver therefore
// backs them with plain, storage-private upstream directories:
//
//	/.c2a-trash/...  <->  /<root>/.goapi-system-<ns>-<id>-trash/...
//	/.relay/...      <->  /<root>/.goapi-system-<ns>-<id>-relay/...
//
// <ns> is a random 128-bit hex namespace generated once at first Init,
// committed only after an adoption probe found both alias paths free, and
// then persisted through OpenList's own storage-save convention (retained
// across re-authorization; not a credential, no wizard input). <id> is the
// storage row id: the namespace keeps OpenList instances sharing one peer
// key apart, the id keeps storages sharing a namespace apart, and mapping
// with id == 0 fails closed. The mapping is bijective and rewrites exactly
// the first segment inside the root, and only when it is exactly one of the
// two reserved names. This storage's own aliases are never directly
// addressable; other storages' aliases pass through untouched; a peer path
// genuinely named /.c2a-trash or /.relay is shadowed by the view; a cleared
// namespace is re-adopted on the next Init; an existing alias path is never
// claimed.

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"

	"github.com/OpenListTeam/OpenList/v4/pkg/goapiclient"
	"github.com/pkg/errors"
)

const (
	virtualTrash = ".c2a-trash"
	virtualRelay = ".relay"
	aliasPrefix  = ".goapi-system-"
	// nsHexLen is the length of the hex form of the 128-bit namespace.
	nsHexLen = 32
)

// newNamespace returns a fresh canonical namespace: 32 lowercase hex chars
// from crypto/rand.
func newNamespace() (string, error) {
	buf := make([]byte, nsHexLen/2)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("goapi: cannot generate the storage system namespace: %s", err)
	}
	return hex.EncodeToString(buf), nil
}

func validNamespace(s string) bool {
	if len(s) != nsHexLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// systemAliases returns the two backing alias names of this storage.
// Mapping is only ever attempted with an established identity — a canonical
// namespace and the storage row id — and fails closed otherwise.
func (d *GoAPI) systemAliases() (trash, relay string, err error) {
	return aliasesFor(strings.TrimSpace(d.Addition.SystemNS), d.Storage.ID)
}

// aliasesFor derives the alias pair from a namespace candidate and the
// storage row id; both must be established.
func aliasesFor(ns string, id uint) (trash, relay string, err error) {
	if !validNamespace(ns) {
		return "", "", errors.New("goapi: the storage system namespace is not established; re-initialize the storage")
	}
	if id == 0 {
		return "", "", errors.New("goapi: the storage row identity is missing; the reserved system view cannot be mapped")
	}
	base := aliasPrefix + ns + "-" + strconv.FormatUint(uint64(id), 10)
	return base + "-trash", base + "-relay", nil
}

func (d *GoAPI) aliasFor(virtual string) (string, error) {
	trash, relay, err := d.systemAliases()
	if err != nil {
		return "", err
	}
	if virtual == virtualTrash {
		return trash, nil
	}
	return relay, nil
}

// firstInsideRoot splits a remote path that has already been validated
// against the configured root into its first segment inside that root.
// first is "" for the root itself; rest always starts with "/" or is empty.
func firstInsideRoot(rootFolderPath, remote string) (first, rest string, err error) {
	rootClean, err := goapiclient.CleanRel(rootFolderPath)
	if err != nil {
		return "", "", err
	}
	rel, err := goapiclient.CleanRel(remote)
	if err != nil {
		return "", "", err
	}
	if rootClean != "" {
		if rel == rootClean {
			return "", "", nil
		}
		if !strings.HasPrefix(rel, rootClean+"/") {
			return "", "", nil
		}
		rel = rel[len(rootClean)+1:]
	}
	if rel == "" {
		return "", "", nil
	}
	if i := strings.IndexByte(rel, '/'); i >= 0 {
		return rel[:i], rel[i:], nil
	}
	return rel, "", nil
}

// remoteInsideRoot rebuilds the remote path for a first segment inside the
// root plus a remainder that starts with "/" or is empty.
func remoteInsideRoot(rootFolderPath, first, rest string) (string, error) {
	rootClean, err := goapiclient.CleanRel(rootFolderPath)
	if err != nil {
		return "", err
	}
	if rootClean == "" {
		return "/" + first + rest, nil
	}
	return "/" + rootClean + "/" + first + rest, nil
}

// mapToPeer rewrites one outbound path from the virtual view onto the peer:
// exactly the first segment inside the root, and only when it is exactly one
// of the two reserved names. Direct addressing of this storage's own backing
// aliases is refused; every other path — including other storages' aliases —
// passes through untouched. Callers must have validated the path against the
// configured root first (d.remotePath).
func (d *GoAPI) mapToPeer(remote string) (string, error) {
	first, rest, err := firstInsideRoot(d.RootFolderPath, remote)
	if err != nil {
		return "", err
	}
	if first == "" {
		return remote, nil
	}
	switch first {
	case virtualTrash, virtualRelay:
		alias, aerr := d.aliasFor(first)
		if aerr != nil {
			return "", aerr
		}
		return remoteInsideRoot(d.RootFolderPath, alias, rest)
	default:
		if trash, relay, aerr := d.systemAliases(); aerr == nil && (first == trash || first == relay) {
			return "", fmt.Errorf("goapi: %q is a storage-managed system directory of this storage and cannot be addressed directly", first)
		}
		return remote, nil
	}
}

// mapFromPeer is the reverse of mapToPeer for entry paths the peer echoed
// back: this storage's own aliases become the reserved virtual names again.
// Aliases of other storages stay the ordinary paths they are; an identity
// that is not established has nothing to rewrite.
func (d *GoAPI) mapFromPeer(peer string) string {
	first, rest, err := firstInsideRoot(d.RootFolderPath, peer)
	if err != nil || first == "" {
		return peer
	}
	trash, relay, aerr := d.systemAliases()
	if aerr != nil {
		return peer
	}
	var virtual string
	switch first {
	case trash:
		virtual = virtualTrash
	case relay:
		virtual = virtualRelay
	default:
		return peer
	}
	mapped, err := remoteInsideRoot(d.RootFolderPath, virtual, rest)
	if err != nil {
		return peer
	}
	return mapped
}
