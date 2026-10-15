package storageid

import (
	"testing"

	"cloud2api/internal/openlist"
)

func st(id int64, mount string, mutate func(*openlist.StorageInfo)) openlist.StorageInfo {
	s := openlist.StorageInfo{ID: id, MountPath: mount, Driver: "Local", Status: "work"}
	if mutate != nil {
		mutate(&s)
	}
	return s
}

func index(list ...openlist.StorageInfo) (map[string]openlist.StorageInfo, map[int64]openlist.StorageInfo) {
	byMount := map[string]openlist.StorageInfo{}
	byID := map[int64]openlist.StorageInfo{}
	for _, s := range list {
		byMount[s.MountPath] = s
		byID[s.ID] = s
	}
	return byMount, byID
}

func TestClassifyStatus(t *testing.T) {
	// ClassifyStatus only sees non-work statuses ("" and "work" are healthy
	// through ClassifyStorage).
	for _, tc := range []struct {
		status string
		want   string
	}{
		{"failed get token: refresh token expired", StateNeedsReauth},
		{"401 unauthorized", StateNeedsReauth},
		{"please login again", StateNeedsReauth},
		{"failed to refresh cookie", StateNeedsReauth},
		{"connection refused to upstream", StateError},
		{"some other failure", StateError},
	} {
		if got := ClassifyStatus(tc.status); got != tc.want {
			t.Errorf("ClassifyStatus(%q) = %q, want %q", tc.status, got, tc.want)
		}
	}
	if got := ClassifyStorage(st(1, "/m", nil)); got != StateHealthy {
		t.Errorf("ClassifyStorage(work) = %q", got)
	}
	if got := ClassifyStorage(st(1, "/m", func(s *openlist.StorageInfo) { s.Disabled = true })); got != StateDisabled {
		t.Errorf("ClassifyStorage(disabled) = %q", got)
	}
	if got := ClassifyStorage(st(1, "/m", func(s *openlist.StorageInfo) { s.Status = "token expired" })); got != StateNeedsReauth {
		t.Errorf("ClassifyStorage(token) = %q", got)
	}
}

// TestResolveTable pins the snapshot classification, in particular that a
// drive with a recorded storage id is NEVER matched by mount path, and that
// replacement_found vs orphaned depends on the candidate at the mount.
func TestResolveTable(t *testing.T) {
	old := st(5, "/d/a", nil)
	cand := st(9, "/d/a", nil)
	other := st(7, "/d/other", nil)

	tests := []struct {
		name     string
		drive    Drive
		storage  []openlist.StorageInfo
		want     string
		wantCand bool
	}{
		{
			name:  "id drive healthy",
			drive: Drive{StorageID: 5, MountPath: "/d/a", Enabled: true},
			want:  StateHealthy,
		},
		{
			name:  "id drive needs_reauth",
			drive: Drive{StorageID: 5, MountPath: "/d/a", Enabled: true},
			want:  StateNeedsReauth,
		},
		{
			name:  "id drive disabled in OpenList",
			drive: Drive{StorageID: 5, MountPath: "/d/a", Enabled: true},
			want:  StateDisabled,
		},
		{
			name:     "id gone, candidate at mount -> replacement_found",
			drive:    Drive{StorageID: 5, MountPath: "/d/a", Enabled: true},
			want:     StateReplacementFound,
			wantCand: true,
		},
		{
			name:  "id gone, mount free -> orphaned",
			drive: Drive{StorageID: 5, MountPath: "/d/a", Enabled: true},
			want:  StateOrphaned,
		},
		{
			// The id drive must never fall back to a DIFFERENT storage at
			// its own mount when some other id sits there.
			name:  "id gone, only unrelated storages -> orphaned",
			drive: Drive{StorageID: 5, MountPath: "/d/a", Enabled: true},
			want:  StateOrphaned,
		},
		{
			name:  "cloud2api-disabled wins",
			drive: Drive{StorageID: 5, MountPath: "/d/a", Enabled: false},
			want:  StateDisabled,
		},
		{
			name:  "id-less static drive matched by mount",
			drive: Drive{MountPath: "/d/a", Static: true, Enabled: true},
			want:  StateHealthy,
		},
		{
			name:  "id-less static drive without storage -> orphaned",
			drive: Drive{MountPath: "/d/a", Static: true, Enabled: true},
			want:  StateOrphaned,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			list := []openlist.StorageInfo{}
			switch tc.name {
			case "id drive healthy":
				list = []openlist.StorageInfo{old}
			case "id drive needs_reauth":
				s := old
				s.Status = "token expired"
				list = []openlist.StorageInfo{s}
			case "id drive disabled in OpenList":
				s := old
				s.Disabled = true
				list = []openlist.StorageInfo{s}
			case "id gone, candidate at mount -> replacement_found":
				list = []openlist.StorageInfo{cand}
			case "id gone, mount free -> orphaned":
				list = nil
			case "id gone, only unrelated storages -> orphaned":
				list = []openlist.StorageInfo{other}
			case "id-less static drive matched by mount":
				list = []openlist.StorageInfo{old}
			case "id-less static drive without storage -> orphaned":
				list = nil
			}
			byMount, byID := index(list...)
			res := Resolve(tc.drive, byMount, byID)
			if res.State != tc.want {
				t.Fatalf("state = %q, want %q", res.State, tc.want)
			}
			if tc.wantCand && (res.Replacement == nil || res.Replacement.ID != 9) {
				t.Fatalf("replacement = %v, want storage 9", res.Replacement)
			}
			if !tc.wantCand && res.Replacement != nil {
				t.Fatalf("unexpected replacement %v", res.Replacement)
			}
		})
	}
}

// TestResolvePersistedReplacementStateWins: once reconcile recorded
// replacement_found, a (stale) snapshot that still lists the old storage must
// not classify the drive as healthy.
func TestResolvePersistedReplacementStateWins(t *testing.T) {
	byMount, byID := index(st(5, "/d/a", nil))
	res := Resolve(Drive{StorageID: 5, MountPath: "/d/a", Enabled: true, State: StateReplacementFound}, byMount, byID)
	if res.State != StateReplacementFound {
		t.Fatalf("state = %q, want replacement_found (persisted state wins over the stale snapshot)", res.State)
	}
}
