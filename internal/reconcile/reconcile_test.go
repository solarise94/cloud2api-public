package reconcile

import (
	"testing"

	"cloud2api/internal/openlist"
)

func snap(id int64, mount, driver string, mutate func(*openlist.StorageInfo)) openlist.StorageInfo {
	st := openlist.StorageInfo{ID: id, MountPath: mount, Driver: driver, Status: "work"}
	if mutate != nil {
		mutate(&st)
	}
	return st
}

func findStep(t *testing.T, steps []Step, id string) Step {
	t.Helper()
	for _, s := range steps {
		if s.DriveID == id {
			return s
		}
	}
	t.Fatalf("no step for drive %s", id)
	return Step{}
}

func hasEvent(s Step, kind string) bool {
	for _, ev := range s.Events {
		if ev.Kind == kind {
			return true
		}
	}
	return false
}

func TestPlanFollowsMountRename(t *testing.T) {
	storages := []openlist.StorageInfo{snap(7, "/renamed", "Local", nil)}
	steps := Plan([]DriveSnap{{
		ID: "d1", Driver: "Local", MountPath: "/orig", StorageID: 7, Enabled: true,
		State: StateHealthy, MountSeen: "/orig",
	}}, storages)
	s := findStep(t, steps, "d1")
	if s.Mount == nil || *s.Mount != "/renamed" {
		t.Fatalf("mount follow = %v, want /renamed", s.Mount)
	}
	if s.MountSeen != "/renamed" {
		t.Fatalf("mount_seen = %s", s.MountSeen)
	}
	if !hasEvent(s, "mount_renamed") {
		t.Fatalf("want mount_renamed event, got %+v", s.Events)
	}
	if s.State != StateHealthy {
		t.Fatalf("state = %s", s.State)
	}

	// Re-running with the followed state (restart of the loop) must not
	// re-emit the event.
	steps = Plan([]DriveSnap{{
		ID: "d1", Driver: "Local", MountPath: "/renamed", StorageID: 7, Enabled: true,
		State: StateHealthy, MountSeen: "/renamed",
	}}, storages)
	s = findStep(t, steps, "d1")
	if hasEvent(s, "mount_renamed") {
		t.Fatalf("mount_renamed must not repeat: %+v", s.Events)
	}

	// A restart that re-applies config.yaml's mount (static drive) with the
	// remembered mount_seen also must not re-emit.
	steps = Plan([]DriveSnap{{
		ID: "d1", Static: true, Driver: "Local", MountPath: "/orig", StorageID: 7, Enabled: true,
		State: StateHealthy, MountSeen: "/renamed",
	}}, storages)
	s = findStep(t, steps, "d1")
	if s.Mount == nil || *s.Mount != "/renamed" {
		t.Fatalf("static drive must still follow, got %v", s.Mount)
	}
	if hasEvent(s, "mount_renamed") {
		t.Fatalf("static restart must not repeat mount_renamed: %+v", s.Events)
	}
}

func TestPlanDisabledInOpenList(t *testing.T) {
	storages := []openlist.StorageInfo{snap(3, "/m", "Local", func(st *openlist.StorageInfo) { st.Disabled = true })}
	steps := Plan([]DriveSnap{{
		ID: "d1", MountPath: "/m", StorageID: 3, Enabled: true, State: StateHealthy, MountSeen: "/m",
	}}, storages)
	s := findStep(t, steps, "d1")
	if s.State != StateDisabledOL {
		t.Fatalf("state = %s, want disabled_openlist", s.State)
	}
	if !hasEvent(s, "disabled_in_openlist") {
		t.Fatalf("want disabled_in_openlist, got %+v", s.Events)
	}
	if DisplayState(s.State) != "disabled" {
		t.Fatalf("display state = %s", DisplayState(s.State))
	}

	// Re-enable.
	storages[0].Disabled = false
	steps = Plan([]DriveSnap{{
		ID: "d1", MountPath: "/m", StorageID: 3, Enabled: true, State: StateDisabledOL, MountSeen: "/m",
	}}, storages)
	s = findStep(t, steps, "d1")
	if s.State != StateHealthy || !hasEvent(s, "enabled_in_openlist") {
		t.Fatalf("state=%s events=%+v", s.State, s.Events)
	}

	// A drive disabled via the cloud2api portal stays silent.
	steps = Plan([]DriveSnap{{
		ID: "d1", MountPath: "/m", StorageID: 3, Enabled: false, State: StateDisabledLoc, MountSeen: "/m",
	}}, storages)
	s = findStep(t, steps, "d1")
	if s.State != StateDisabledLoc || len(s.Events) != 0 {
		t.Fatalf("local disable must be silent: state=%s events=%+v", s.State, s.Events)
	}
}

func TestPlanDeletedStorage(t *testing.T) {
	steps := Plan([]DriveSnap{{
		ID: "d1", MountPath: "/m", StorageID: 3, Enabled: true, State: StateHealthy, MountSeen: "/m",
	}}, nil)
	s := findStep(t, steps, "d1")
	if s.State != StateOrphaned {
		t.Fatalf("state = %s", s.State)
	}
	if !hasEvent(s, "deleted_in_openlist") {
		t.Fatalf("want deleted_in_openlist, got %+v", s.Events)
	}

	// Already orphaned → no repeat.
	steps = Plan([]DriveSnap{{
		ID: "d1", MountPath: "/m", StorageID: 3, Enabled: true, State: StateOrphaned, MountSeen: "/m",
	}}, nil)
	s = findStep(t, steps, "d1")
	if hasEvent(s, "deleted_in_openlist") {
		t.Fatalf("deleted_in_openlist must not repeat")
	}
}

func TestPlanReplacementNeedsConfirmation(t *testing.T) {
	// Storage 5 deleted; a new storage 9 took over the mount. The drive must
	// NOT rebind: state replacement_found, storage id untouched, event emitted
	// once (IMPLEMENTATION §存储被删除与替代存储).
	storages := []openlist.StorageInfo{snap(9, "/m", "Aliyun", nil)}
	d := DriveSnap{
		ID: "d1", MountPath: "/m", StorageID: 5, Enabled: true, State: StateHealthy, MountSeen: "/m",
	}
	s := findStep(t, Plan([]DriveSnap{d}, storages), "d1")
	if s.StorageID != nil {
		t.Fatalf("storage id must not change automatically, got %v", *s.StorageID)
	}
	if s.State != StateReplacementFound {
		t.Fatalf("state = %s, want replacement_found", s.State)
	}
	if !hasEvent(s, "replacement_found") {
		t.Fatalf("want replacement_found event, got %+v", s.Events)
	}
	if s.Mount != nil {
		t.Fatalf("mount must not follow the candidate, got %v", *s.Mount)
	}

	// Second pass while still unconfirmed: state kept, event deduped.
	d.State = StateReplacementFound
	s = findStep(t, Plan([]DriveSnap{d}, storages), "d1")
	if s.State != StateReplacementFound || hasEvent(s, "replacement_found") {
		t.Fatalf("replacement_found must not repeat: state=%s events=%+v", s.State, s.Events)
	}

	// Candidate removed again → orphaned (with the deleted_in_openlist
	// transition event).
	s = findStep(t, Plan([]DriveSnap{d}, nil), "d1")
	if s.State != StateOrphaned {
		t.Fatalf("state = %s, want orphaned after the candidate vanished", s.State)
	}
	if !hasEvent(s, "deleted_in_openlist") {
		t.Fatalf("want deleted_in_openlist transition, got %+v", s.Events)
	}

	// Static drives with a recorded id follow the same confirmation rule.
	s = findStep(t, Plan([]DriveSnap{{
		ID: "d1", Static: true, MountPath: "/m", StorageID: 5, Enabled: true, State: StateHealthy, MountSeen: "/m",
	}}, storages), "d1")
	if s.State != StateReplacementFound || s.StorageID != nil {
		t.Fatalf("static drive: state=%s storageID=%v, want replacement_found and no rebind", s.State, s.StorageID)
	}
}

func TestPlanStaticAutoAdopt(t *testing.T) {
	storages := []openlist.StorageInfo{snap(1, "/baidu", "Local", nil)}
	steps := Plan([]DriveSnap{{
		ID: "baidu-main", Static: true, MountPath: "/baidu", StorageID: 0, Enabled: true, State: "",
	}}, storages)
	s := findStep(t, steps, "baidu-main")
	if s.StorageID == nil || *s.StorageID != 1 {
		t.Fatalf("auto-adopt storage id = %v", s.StorageID)
	}
	if !hasEvent(s, "adopted") {
		t.Fatalf("want adopted event, got %+v", s.Events)
	}

	// Second boot: id recorded → silent.
	steps = Plan([]DriveSnap{{
		ID: "baidu-main", Static: true, MountPath: "/baidu", StorageID: 1, Enabled: true, State: StateHealthy, MountSeen: "/baidu",
	}}, storages)
	s = findStep(t, steps, "baidu-main")
	if hasEvent(s, "adopted") || s.StorageID != nil {
		t.Fatalf("adopted must fire once: %+v %v", s.Events, s.StorageID)
	}
}

func TestPlanNeedsReauthClassification(t *testing.T) {
	storages := []openlist.StorageInfo{snap(6, "/m", "BaiduNetdisk", func(st *openlist.StorageInfo) {
		st.Status = "failed get token: refresh token expired"
	})}
	s := findStep(t, Plan([]DriveSnap{{
		ID: "d1", MountPath: "/m", StorageID: 6, Enabled: true, State: StateHealthy, MountSeen: "/m",
	}}, storages), "d1")
	if s.State != StateNeedsReauth {
		t.Fatalf("state = %s, want needs_reauth", s.State)
	}
}

// TestPlanFollowsStorageDriver (Phase 2 fix): the storage's driver is the
// truth; a row that guessed the driver from the provider template is
// corrected with a driver_changed event, and stable rows produce no event.
func TestPlanFollowsStorageDriver(t *testing.T) {
	storages := []openlist.StorageInfo{snap(7, "/m", "Local", nil)}
	d := DriveSnap{ID: "d1", Provider: "baidu", Driver: "BaiduNetdisk", MountPath: "/m", StorageID: 7, Enabled: true, State: StateHealthy, MountSeen: "/m"}
	s := findStep(t, Plan([]DriveSnap{d}, storages), "d1")
	if s.Driver == nil || *s.Driver != "Local" {
		t.Fatalf("driver step = %v, want Local", s.Driver)
	}
	if !hasEvent(s, "driver_changed") {
		t.Fatalf("want driver_changed event, got %+v", s.Events)
	}
	// Already correct: no step, no event.
	d.Driver = "Local"
	s = findStep(t, Plan([]DriveSnap{d}, storages), "d1")
	if s.Driver != nil || hasEvent(s, "driver_changed") {
		t.Fatalf("stable driver produced a step: %+v", s)
	}
}
