// Package reconcile implements the OpenList ↔ cloud2api reconciliation loop
// (UPGRADE §3.7.4): drives are keyed by OpenList storage id, mount renames in
// OpenList are followed, storages disabled or deleted in OpenList flip the
// drive state, unknown storages surface as unmanaged, and a new storage at a
// deleted drive's mount waits for administrator confirmation. OpenList is the
// single source of truth for storage configuration — this loop only tracks
// identity and state, it never writes storage config.
package reconcile

import (
	"context"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"cloud2api/internal/gateway"
	"cloud2api/internal/openlist"
	"cloud2api/internal/storageid"
	"cloud2api/internal/store"
)

// Interval of the background reconcile pass (§3.7.4: every 5 minutes).
const Interval = 5 * time.Minute

// eventRetention bounds the linkage event log.
const eventRetention = 30 * 24 * time.Hour

// Reconciler owns the reconcile pass. All entry points are safe for
// concurrent use; passes serialize on mu.
type Reconciler struct {
	st       *store.Store
	registry *gateway.AccountRegistry
	client   *openlist.Client
	log      *slog.Logger

	// AfterPass runs after every successful pass (cache invalidation hooks
	// installed by main: portal storage cache, agent health cache).
	AfterPass func()

	mu sync.Mutex

	trigMu     sync.Mutex
	trigActive bool
}

func New(st *store.Store, registry *gateway.AccountRegistry, client *openlist.Client, logger *slog.Logger) *Reconciler {
	if logger == nil {
		logger = slog.Default()
	}
	return &Reconciler{st: st, registry: registry, client: client, log: logger}
}

// Start launches the background loop: one immediate pass, then a pass every
// Interval until ctx is done.
func (r *Reconciler) Start(ctx context.Context) {
	go func() {
		pctx, cancel := context.WithTimeout(ctx, 45*time.Second)
		if err := r.Reconcile(pctx); err != nil {
			r.log.Warn("initial reconcile failed", "err", err)
		}
		cancel()
		t := time.NewTicker(Interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				pctx, cancel := context.WithTimeout(ctx, 45*time.Second)
				if err := r.Reconcile(pctx); err != nil {
					r.log.Warn("periodic reconcile failed", "err", err)
				}
				cancel()
			}
		}
	}()
}

// Trigger schedules one asynchronous pass (deduplicated: at most one pending
// at a time). Used when the portal lists drives and when the data plane hits
// storage-not-found / mount-mismatch conditions.
func (r *Reconciler) Trigger(reason string) {
	r.trigMu.Lock()
	if r.trigActive {
		r.trigMu.Unlock()
		return
	}
	r.trigActive = true
	r.trigMu.Unlock()
	go func() {
		defer func() {
			r.trigMu.Lock()
			r.trigActive = false
			r.trigMu.Unlock()
		}()
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()
		if err := r.Reconcile(ctx); err != nil {
			r.log.Warn("reconcile triggered failed", "reason", reason, "err", err)
		}
	}()
}

// Reconcile runs one pass: read a fresh OpenList storage list (paged), plan
// the transitions and apply them (store rows, registry mounts, events).
func (r *Reconciler) Reconcile(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	storages, err := r.client.ListStorages(ctx)
	if err != nil {
		return err
	}
	return r.reconcileWithLocked(ctx, storages)
}

// ReconcileWith runs one pass on a given storage snapshot (IMPLEMENTATION
// §刷新与纳管: the sync endpoint reads one snapshot and drives everything
// from it). Same locking and post-pass hooks as Reconcile.
func (r *Reconciler) ReconcileWith(ctx context.Context, storages []openlist.StorageInfo) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.reconcileWithLocked(ctx, storages)
}

func (r *Reconciler) reconcileWithLocked(ctx context.Context, storages []openlist.StorageInfo) error {
	drives, err := r.st.ListDrives()
	if err != nil {
		return err
	}
	snaps := make([]DriveSnap, 0, len(drives))
	for _, d := range drives {
		snaps = append(snaps, DriveSnap{
			ID: d.ID, Provider: d.Provider, Driver: d.Driver, MountPath: d.MountPath,
			StorageID: d.StorageID, Enabled: d.Enabled, Static: d.Static,
			State: d.State, MountSeen: d.MountSeen,
		})
	}
	for _, step := range Plan(snaps, storages) {
		if err := r.apply(ctx, step); err != nil {
			r.log.Warn("reconcile step failed", "drive", step.DriveID, "err", err)
		}
	}
	if err := r.st.PruneEvents(eventRetention); err != nil {
		r.log.Warn("event prune failed", "err", err)
	}
	if r.AfterPass != nil {
		r.AfterPass()
	}
	return nil
}

func (r *Reconciler) apply(ctx context.Context, step Step) error {
	if err := r.st.ApplyReconcile(step.DriveID, step.State, step.MountSeen, step.Mount, step.StorageID); err != nil {
		return err
	}
	if step.Driver != nil {
		if err := r.st.SetDriveDriver(step.DriveID, *step.Driver); err != nil {
			return err
		}
	}
	if step.Mount != nil {
		if err := r.registry.SetMount(step.DriveID, *step.Mount); err != nil {
			r.log.Warn("registry mount follow failed", "drive", step.DriveID, "mount", *step.Mount, "err", err)
		}
	}
	for _, ev := range step.Events {
		if err := r.st.InsertEvent(ev.Drive, ev.Kind, ev.Detail); err != nil {
			r.log.Warn("event insert failed", "drive", ev.Drive, "kind", ev.Kind, "err", err)
		}
	}
	return nil
}

// ---- pure planning (unit tested) ----

// Event is one linkage event to record.
type Event struct {
	Drive  string
	Kind   string
	Detail string
}

// DriveSnap is the reconcile input for one drive.
type DriveSnap struct {
	ID        string
	Provider  string
	Driver    string
	MountPath string
	StorageID int64
	Enabled   bool
	Static    bool
	State     string // last persisted granular state ("" = never reconciled)
	MountSeen string // OpenList mount last observed (event dedupe)
}

// Step is the reconcile output for one drive.
type Step struct {
	DriveID   string
	Events    []Event
	Mount     *string // non-nil → follow the OpenList mount
	StorageID *int64  // non-nil → (re)bind the storage id
	Driver    *string // non-nil → adopt the storage's real driver (Phase 2 fix)
	State     string  // granular state to persist
	MountSeen string
}

// granular states persisted in drives.state (display maps disabled_* to
// "disabled"; see store.Drive usage in admin).
const (
	StateHealthy          = "healthy"
	StateNeedsReauth      = "needs_reauth"
	StateError            = "error"
	StateDisabledLoc      = "disabled_local"    // disabled via the cloud2api portal
	StateDisabledOL       = "disabled_openlist" // storage disabled in OpenList
	StateOrphaned         = "orphaned"
	StateReplacementFound = "replacement_found" // recorded storage gone, unconfirmed candidate at the mount
)

// DisplayState maps a granular persisted state onto the §8.2 vocabulary.
func DisplayState(state string) string {
	if state == StateDisabledLoc || state == StateDisabledOL {
		return "disabled"
	}
	return state
}

// Plan computes the transitions for every drive given a fresh storage list.
// It is pure: no I/O, no mutation.
func Plan(drives []DriveSnap, storages []openlist.StorageInfo) []Step {
	byID := make(map[int64]openlist.StorageInfo, len(storages))
	byMount := make(map[string]openlist.StorageInfo, len(storages))
	for _, st := range storages {
		byMount[st.MountPath] = st
		if st.ID != 0 {
			byID[st.ID] = st
		}
	}

	steps := make([]Step, 0, len(drives))
	for _, d := range drives {
		step := Step{DriveID: d.ID, MountSeen: d.MountSeen}

		var st openlist.StorageInfo
		var matched bool
		if d.StorageID != 0 {
			st, matched = byID[d.StorageID]
		}
		if !matched && d.StorageID == 0 {
			// Mount matching is reserved for drives that never recorded a
			// storage id (static config.yaml account, first-boot adoption).
			var byMountHit bool
			st, byMountHit = byMount[d.MountPath]
			if byMountHit {
				matched = true
				newID := st.ID
				step.StorageID = &newID
				if d.Static {
					// First boot auto-adopt of a config.yaml account (§3.7.4).
					step.Events = append(step.Events, Event{d.ID, "adopted",
						"static drive adopted storage " + itoa(st.ID) + " by mount path " + d.MountPath})
				}
			}
		}
		if !matched && d.StorageID != 0 {
			// The recorded storage is gone. Another storage at the drive's
			// mount is an UNCONFIRMED replacement (deleted + recreated in
			// OpenList, possibly another account or driver): never rebind
			// automatically — the admin confirms via the portal rebind, and
			// until then the data plane denies access.
			if cand, ok := byMount[d.MountPath]; ok {
				step.State = StateReplacementFound
				if d.State != StateReplacementFound {
					step.Events = append(step.Events, Event{d.ID, "replacement_found",
						"storage " + itoa(d.StorageID) + " is gone; candidate storage " + itoa(cand.ID) +
							" (driver " + cand.Driver + ") sits at mount " + d.MountPath +
							"; waiting for administrator confirmation before rebinding"})
				}
				steps = append(steps, step)
				continue
			}
			step.State = StateOrphaned
			if d.State != "" && d.State != StateOrphaned {
				step.Events = append(step.Events, Event{d.ID, "deleted_in_openlist",
					"storage " + itoa(d.StorageID) + " (mount " + d.MountPath + ") no longer exists in OpenList; drive is orphaned"})
			}
			steps = append(steps, step)
			continue
		}

		if !matched {
			step.State = StateOrphaned
			if d.State != "" && d.State != StateOrphaned {
				step.Events = append(step.Events, Event{d.ID, "deleted_in_openlist",
					"storage " + itoa(d.StorageID) + " (mount " + d.MountPath + ") no longer exists in OpenList; drive is orphaned"})
			}
			steps = append(steps, step)
			continue
		}

		// Mount follow: OpenList renamed the mount path (§3.7.4).
		step.MountSeen = st.MountPath
		if st.MountPath != d.MountPath {
			mp := st.MountPath
			step.Mount = &mp
			if d.MountSeen != st.MountPath {
				step.Events = append(step.Events, Event{d.ID, "mount_renamed",
					"mount renamed in OpenList: " + d.MountPath + " -> " + st.MountPath + " (followed; drive id and key grants unchanged)"})
			}
		}

		// Driver follow (Phase 2 fix): the storage's driver is the truth —
		// config.yaml-era rows guessed it from the provider template, which
		// is wrong for simulated netdisks (provider baidu on a Local
		// storage). The provider label stays as the cloud2api display name.
		// An empty row driver is filled in silently (nothing to correct).
		if st.Driver != "" && d.Driver != "" && st.Driver != d.Driver {
			dr := st.Driver
			step.Driver = &dr
			step.Events = append(step.Events, Event{d.ID, "driver_changed",
				"storage " + itoa(st.ID) + " reports driver " + st.Driver + " (row had " + d.Driver + "); drive metadata now uses the real driver"})
		} else if st.Driver != "" && d.Driver == "" {
			dr := st.Driver
			step.Driver = &dr
		}

		// State.
		switch {
		case !d.Enabled:
			step.State = StateDisabledLoc
		case st.Disabled:
			step.State = StateDisabledOL
			if d.State != "" && d.State != StateDisabledOL && d.State != StateDisabledLoc && d.State != StateOrphaned {
				step.Events = append(step.Events, Event{d.ID, "disabled_in_openlist",
					"storage " + itoa(st.ID) + " was disabled in OpenList; /v1 returns 503 drive_unavailable"})
			}
		case st.Status == "work":
			step.State = StateHealthy
		default:
			step.State = classify(st.Status)
		}
		if d.State == StateDisabledOL && (step.State == StateHealthy || step.State == StateNeedsReauth || step.State == StateError) {
			step.Events = append(step.Events, Event{d.ID, "enabled_in_openlist",
				"storage " + itoa(st.ID) + " was re-enabled in OpenList"})
		}

		steps = append(steps, step)
	}
	return steps
}

func classify(status string) string {
	return storageid.ClassifyStatus(status)
}

func itoa(v int64) string {
	if v == 0 {
		return "?"
	}
	return strconv.FormatInt(v, 10)
}
