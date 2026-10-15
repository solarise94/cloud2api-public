// Package storageid owns the drive ↔ OpenList-storage identity mapping and
// the state vocabulary derived from it (IMPLEMENTATION §存储被删除与替代存储).
//
// A drive tracks its OpenList storage BY STORAGE ID. Only a drive that never
// recorded an id (a static config.yaml account before its first reconcile
// pass) may be matched by mount path. When a drive's recorded storage is gone
// and a new storage sits at the drive's mount, the state is replacement_found
// and access is denied until an administrator confirms the replacement — the
// new storage may belong to a different netdisk account or even a different
// driver, so the old key grants must not carry over silently.
package storageid

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"cloud2api/internal/openlist"
)

// Drive states (the §8.2 health vocabulary plus replacement_found and the
// guard-internal unverified).
const (
	StateHealthy          = "healthy"
	StateNeedsReauth      = "needs_reauth"
	StateError            = "error"
	StateDisabled         = "disabled"
	StateOrphaned         = "orphaned"
	StateReplacementFound = "replacement_found"
	StateUnverified       = "unverified" // guard-only: identity could not be verified, deny
)

// Drive is the identity-relevant subset of a store drive row.
type Drive struct {
	StorageID int64
	MountPath string
	Static    bool
	Enabled   bool
	State     string // last persisted reconcile state ("" = never reconciled)
}

// ClassifyStatus maps an OpenList storage status text onto the health
// vocabulary: token/401/login/expired class failures are needs_reauth,
// everything else (except "work") is error.
func ClassifyStatus(status string) string {
	low := strings.ToLower(status)
	for _, kw := range []string{"token", "401", "unauthorized", "login", "expired", "refresh"} {
		if strings.Contains(low, kw) {
			return StateNeedsReauth
		}
	}
	return StateError
}

// ClassifyStorage derives the state of a matched storage.
func ClassifyStorage(st openlist.StorageInfo) string {
	if st.Disabled {
		return StateDisabled
	}
	if st.Status == "" || st.Status == "work" {
		return StateHealthy
	}
	return ClassifyStatus(st.Status)
}

// Result is a snapshot classification of one drive (display surfaces such as
// /v1/me; the data plane uses Guard.Check instead).
type Result struct {
	State      string
	StatusText string // raw OpenList status of the matched storage

	// Storage is the storage matched by id (or, for a drive that never
	// recorded an id, by mount path); nil when nothing matched.
	Storage *openlist.StorageInfo

	// Replacement is the storage currently sitting at the drive's mount path
	// while the recorded id is gone; nil unless State is replacement_found.
	Replacement *openlist.StorageInfo
}

// Resolve classifies a drive against a storage snapshot. A drive with a
// recorded storage id is matched ONLY by that id; the mount path is consulted
// solely to name a replacement candidate. A drive without an id (static
// config.yaml account before adoption) is matched by mount path.
//
// The persisted reconcile State wins over a snapshot that still lists a
// storage a fresher reconcile pass observed as gone (a stale display cache):
// once replacement_found is recorded, the drive stays replacement_found until
// the id is rebound.
func Resolve(d Drive, byMount map[string]openlist.StorageInfo, byID map[int64]openlist.StorageInfo) Result {
	if !d.Enabled {
		return Result{State: StateDisabled, StatusText: "disabled in cloud2api"}
	}
	if d.StorageID != 0 {
		if d.State == StateReplacementFound {
			// A reconcile pass fresher than this (possibly stale) snapshot
			// observed the recorded id gone: keep replacement_found until
			// the id is rebound, naming the candidate when one is visible.
			if cand, ok := byMount[d.MountPath]; ok && cand.ID != d.StorageID {
				return Result{State: StateReplacementFound, Replacement: &cand}
			}
			return Result{State: StateReplacementFound}
		}
		if st, ok := byID[d.StorageID]; ok {
			return Result{State: ClassifyStorage(st), StatusText: st.Status, Storage: &st}
		}
		if cand, ok := byMount[d.MountPath]; ok && cand.ID != d.StorageID {
			return Result{State: StateReplacementFound, Replacement: &cand}
		}
		return Result{State: StateOrphaned}
	}
	if st, ok := byMount[d.MountPath]; ok {
		return Result{State: ClassifyStorage(st), StatusText: st.Status, Storage: &st}
	}
	return Result{State: StateOrphaned}
}

// Verdict is the outcome of one Guard.Check.
type Verdict struct {
	State      string
	StatusText string // raw OpenList status of the matched storage

	// Message is the human/agent-facing reason; empty for allowed states.
	Message string

	// Storage is the storage the verdict was derived from; nil when no
	// storage was matched.
	Storage *openlist.StorageInfo

	// Replacement names the candidate storage when State is
	// replacement_found; nil otherwise.
	Replacement *openlist.StorageInfo
}

// Allowed reports whether the request may proceed to the OpenList fs call.
// error stays allowed (upstream errors surface as they do today); every other
// non-healthy state is denied.
func (v Verdict) Allowed() bool {
	return v.State == StateHealthy || v.State == StateError
}

// guardTimeout bounds one Check including its bounded reconcile hook run.
const guardTimeout = 25 * time.Second

// Guard performs a FRESH per-request verification of a drive's backing
// storage, used immediately before any OpenList file access. Drive identity
// (storage id, enabled flag) comes from the store lookup; the mount path that
// will actually be hit comes from the mount lookup (the live account
// registry). Neither is cached here.
type Guard struct {
	client    *openlist.Client
	lookup    func(driveID string) (Drive, bool) // nil → every drive is id-less
	mount     func(driveID string) (string, bool)
	reconcile func(context.Context) // bounded synchronous hook, optional
	log       *slog.Logger
}

// NewGuard builds a guard. lookup may be nil (identity falls back to
// id-less, mount-matched semantics — the legacy gateway default); mount
// should resolve the drive to the mount path the data plane will address.
func NewGuard(client *openlist.Client, lookup func(string) (Drive, bool), mount func(string) (string, bool), logger *slog.Logger) *Guard {
	if logger == nil {
		logger = slog.Default()
	}
	return &Guard{client: client, lookup: lookup, mount: mount, log: logger}
}

// SetReconciler installs the bounded synchronous reconcile hook run when the
// recorded storage is missing or its mount moved (it follows mount renames
// and updates the registry the mount lookup reads).
func (g *Guard) SetReconciler(f func(context.Context)) { g.reconcile = f }

func (g *Guard) driveOf(driveID string) Drive {
	if g.lookup != nil {
		if d, ok := g.lookup(driveID); ok {
			return d
		}
	}
	// Unknown to the store (or no lookup wired): id-less semantics.
	return Drive{Enabled: true}
}

func (g *Guard) mountOf(driveID string, fallback string) string {
	if g.mount != nil {
		if m, ok := g.mount(driveID); ok && m != "" {
			return m
		}
	}
	return fallback
}

func (g *Guard) runReconcile(ctx context.Context) {
	if g.reconcile == nil {
		return
	}
	g.reconcile(ctx)
}

func (g *Guard) triggerReconcile() {
	if g.reconcile == nil {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), guardTimeout)
		defer cancel()
		g.reconcile(ctx)
	}()
}

// Check verifies driveID's backing storage with fresh OpenList reads (no
// cache). The residual race — the storage being swapped between this check
// and the fs call milliseconds later — is accepted; the window is the price
// of not serializing every request behind the admin API.
func (g *Guard) Check(ctx context.Context, driveID string) Verdict {
	ctx, cancel := context.WithTimeout(ctx, guardTimeout)
	defer cancel()

	d := g.driveOf(driveID)
	if !d.Enabled {
		return Verdict{State: StateDisabled,
			Message: "the drive is disabled in cloud2api; ask the administrator to re-enable it"}
	}
	mount := g.mountOf(driveID, d.MountPath)

	if d.StorageID != 0 {
		return g.checkByID(ctx, driveID, d, mount)
	}
	return g.checkByMount(ctx, driveID, mount)
}

// checkByID: only the recorded storage id counts (IMPLEMENTATION
// §存储被删除与替代存储 — the mount fallback is reserved for id-less drives).
func (g *Guard) checkByID(ctx context.Context, driveID string, d Drive, mount string) Verdict {
	st, err := g.client.GetStorage(ctx, d.StorageID)
	if err != nil {
		if isStorageNotFound(err) {
			// The recorded storage is gone: let reconcile record the
			// transition (replacement_found / orphaned), then classify from a
			// fresh full list.
			g.runReconcile(ctx)
			list, lerr := g.client.ListStorages(ctx)
			if lerr != nil {
				return unverified(lerr)
			}
			for i := range list {
				if list[i].ID == d.StorageID {
					// The storage came back between the two reads (re-created
					// with the same id): classify it.
					return classify(driveID, list[i])
				}
			}
			for i := range list {
				if list[i].MountPath == mount {
					cand := list[i]
					return Verdict{State: StateReplacementFound, Replacement: &cand,
						Message: "the drive's OpenList storage was replaced by a new storage that an administrator has not confirmed yet; access is blocked until it is confirmed in the cloud2api portal"}
				}
			}
			return Verdict{State: StateOrphaned,
				Message: "the backing OpenList storage was deleted; an administrator must re-create it in OpenList and confirm it in the cloud2api portal"}
		}
		return unverified(err)
	}
	if st.MountPath != mount {
		// The storage moved: reconcile follows mount renames and updates the
		// registry; re-read the registry mount and the storage once.
		g.runReconcile(ctx)
		mount = g.mountOf(driveID, d.MountPath)
		st2, err := g.client.GetStorage(ctx, d.StorageID)
		if err != nil {
			if isStorageNotFound(err) {
				return g.checkByID(ctx, driveID, d, mount)
			}
			return unverified(err)
		}
		if st2.MountPath != mount {
			return Verdict{State: StateUnverified,
				Message: "cloud2api could not verify the drive's OpenList storage (storage " + itoa(d.StorageID) + " is mounted at " + st2.MountPath + ", not " + mount + "); try again shortly"}
		}
		return classify(driveID, st2)
	}
	return classify(driveID, st)
}

// checkByMount: id-less drive (static config.yaml account before its first
// reconcile adoption). Matched by the registry mount path.
func (g *Guard) checkByMount(ctx context.Context, driveID string, mount string) Verdict {
	list, err := g.client.ListStorages(ctx)
	if err != nil {
		return unverified(err)
	}
	find := func() *openlist.StorageInfo {
		for i := range list {
			if list[i].MountPath == mount {
				st := list[i]
				return &st
			}
		}
		return nil
	}
	if st := find(); st != nil {
		// Found by mount (first-boot semantics): adopt the id asynchronously
		// via reconcile so later requests bind to the id.
		g.triggerReconcile()
		return classify(driveID, *st)
	}
	// Not at the registry mount: the mount may have been renamed in OpenList
	// (or the storage deleted). Reconcile follows renames, then re-match
	// against the (possibly updated) registry mount.
	g.runReconcile(ctx)
	if list, err = g.client.ListStorages(ctx); err != nil {
		return unverified(err)
	}
	mount = g.mountOf(driveID, mount)
	if st := find(); st != nil {
		return classify(driveID, *st)
	}
	return Verdict{State: StateOrphaned,
		Message: "the backing OpenList storage was deleted; an administrator must re-create it in OpenList and confirm it in the cloud2api portal"}
}

func classify(driveID string, st openlist.StorageInfo) Verdict {
	v := Verdict{State: ClassifyStorage(st), StatusText: st.Status, Storage: &st}
	switch v.State {
	case StateNeedsReauth:
		v.Message = "the netdisk rejected its stored credentials (" + statusOr(st.Status) + "); ask an administrator to update this drive's login credentials in OpenList, then refresh the drive in the cloud2api portal"
	case StateDisabled:
		v.Message = "the storage is disabled in OpenList; an administrator must re-enable it"
	}
	return v
}

func unverified(err error) Verdict {
	return Verdict{State: StateUnverified,
		Message: "cloud2api could not verify the drive's OpenList storage (" + err.Error() + "); try again shortly"}
}

func isStorageNotFound(err error) bool {
	return errors.Is(err, openlist.ErrStorageNotFound)
}

func statusOr(s string) string {
	if s == "" {
		return "storage unavailable"
	}
	return s
}

func itoa(v int64) string {
	if v == 0 {
		return "?"
	}
	return strconv.FormatInt(v, 10)
}
