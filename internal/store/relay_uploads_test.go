package store

import (
	"errors"
	"sync"
	"testing"
	"time"
)

func mustReserve(t *testing.T, s *Store, id, key string) {
	t.Helper()
	if err := s.ReserveRelayUpload(id, key, "", "", 0); err != nil {
		t.Fatal(err)
	}
}

func TestRelayUploadReceiptLifecycle(t *testing.T) {
	s, err := Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// Staging binds the chunk uuid via a metadata update, then completion
	// adopts the receipt without resetting it.
	mustReserve(t, s, "upl-0001", "k1")
	if err := s.UpdateRelayUploadMeta("upl-0001", "k1", "uuid-1", "a.bin", 10); err != nil {
		t.Fatal(err)
	}
	if err := s.AdoptRelayUpload("upl-0001", "k1", "uuid-1", "a.bin", 10); err != nil {
		t.Fatal(err)
	}
	r, err := s.GetRelayUpload("upl-0001")
	if err != nil {
		t.Fatal(err)
	}
	if r.State != RelayUploadUploading || r.KeyID != "k1" || r.UUID != "uuid-1" || r.Filename != "a.bin" || r.Filesize != 10 {
		t.Fatalf("adopted = %+v", r)
	}

	// Terminal success records the relay row's proof fields.
	row := RelayFile{ID: "file1", Size: 10, CRC64: "42", SHA256: "aa"}
	if err := s.FinishRelayUpload("upl-0001", RelayUploadCompleted, "ok", &row); err != nil {
		t.Fatal(err)
	}
	r, _ = s.GetRelayUpload("upl-0001")
	if r.State != RelayUploadCompleted || r.FileID != "file1" || r.Size != 10 || r.CRC64 != "42" {
		t.Fatalf("completed = %+v", r)
	}

	// Monotone outcomes: neither a late failure nor a duplicate completion
	// may overwrite the truthful terminal state; adopt cannot revive it.
	if err := s.FinishRelayUpload("upl-0001", RelayUploadFailed, "late callback", nil); !errors.Is(err, ErrReceiptTerminal) {
		t.Fatalf("late failure overwrite = %v, want ErrReceiptTerminal", err)
	}
	if err := s.FinishRelayUpload("upl-0001", RelayUploadCompleted, "again", &row); !errors.Is(err, ErrReceiptTerminal) {
		t.Fatalf("duplicate completion overwrite = %v, want ErrReceiptTerminal", err)
	}
	if err := s.AdoptRelayUpload("upl-0001", "k1", "uuid-2", "b.bin", 5); !errors.Is(err, ErrReceiptTerminal) {
		t.Fatalf("terminal adopt = %v, want ErrReceiptTerminal", err)
	}
	r, _ = s.GetRelayUpload("upl-0001")
	if r.State != RelayUploadCompleted || r.Message != "ok" || r.FileID != "file1" {
		t.Fatalf("terminal receipt mutated: %+v", r)
	}
	// Non-terminal finishes are refused outright.
	if err := s.FinishRelayUpload("upl-0001", RelayUploadUploading, "", nil); err == nil {
		t.Fatal("non-terminal finish accepted")
	}

	// Attempt identity is immutable: the same key may not re-reserve its own
	// id (a retry needs a fresh id), another key may never take any used id.
	err = s.ReserveRelayUpload("upl-0001", "k1", "", "", 0)
	if !errors.Is(err, ErrUploadIDExists) {
		t.Fatalf("same-key re-reserve = %v, want ErrUploadIDExists", err)
	}
	err = s.ReserveRelayUpload("upl-0001", "k2", "", "", 0)
	if !errors.Is(err, ErrUploadIDTaken) {
		t.Fatalf("cross-key re-reserve = %v, want ErrUploadIDTaken", err)
	}
	err = s.AdoptRelayUpload("upl-0001", "k2", "", "", 0)
	if !errors.Is(err, ErrUploadIDTaken) {
		t.Fatalf("cross-key adopt = %v, want ErrUploadIDTaken", err)
	}

	// Unknown ids are ErrNotFound; uuid lookup is owner-scoped.
	if _, err := s.GetRelayUpload("ghost"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("ghost = %v", err)
	}
	if _, err := s.RelayUploadByUUID("k2", "uuid-1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign uuid lookup = %v, want not found", err)
	}
	if got, err := s.RelayUploadByUUID("k1", "uuid-1"); err != nil || got.ID != "upl-0001" {
		t.Fatalf("uuid lookup = %+v err=%v", got, err)
	}
}

// TestRelayUploadClaim pins the atomic completion claim: exactly one claim
// per uploading attempt, claims survive metadata updates, terminal receipts
// are never claimable, and attempts cannot be re-bound across identities.
func TestRelayUploadClaim(t *testing.T) {
	s, err := Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	mustReserve(t, s, "cl-0001", "k1")
	if err := s.UpdateRelayUploadMeta("cl-0001", "k1", "uuid-a", "a.bin", 5); err != nil {
		t.Fatal(err)
	}
	// A receipt not bound to the presented chunk uuid cannot be adopted.
	if err := s.AdoptRelayUpload("cl-0001", "k1", "uuid-b", "b.bin", 5); !errors.Is(err, ErrUploadAttemptMismatch) {
		t.Fatalf("uuid mismatch adopt = %v, want ErrUploadAttemptMismatch", err)
	}
	// A direct-upload receipt (no uuid) can never be adopted as a chunk attempt.
	mustReserve(t, s, "cl-direct", "k1")
	if err := s.AdoptRelayUpload("cl-direct", "k1", "uuid-a", "a.bin", 5); !errors.Is(err, ErrUploadAttemptMismatch) {
		t.Fatalf("direct adopt = %v, want ErrUploadAttemptMismatch", err)
	}

	// First claim wins; the second is refused even for the matching uuid.
	if err := s.ClaimRelayUpload("cl-0001", "k1"); err != nil {
		t.Fatal(err)
	}
	if err := s.ClaimRelayUpload("cl-0001", "k1"); !errors.Is(err, ErrReceiptClaimed) {
		t.Fatalf("second claim = %v, want ErrReceiptClaimed", err)
	}
	if err := s.AdoptRelayUpload("cl-0001", "k1", "uuid-a", "a.bin", 5); !errors.Is(err, ErrReceiptClaimed) {
		t.Fatalf("adopt of claimed = %v, want ErrReceiptClaimed", err)
	}
	// Metadata updates never reset the claim or the state.
	if err := s.UpdateRelayUploadMeta("cl-0001", "k1", "uuid-a", "a2.bin", 6); err != nil {
		t.Fatal(err)
	}
	r, _ := s.GetRelayUpload("cl-0001")
	if !r.Claimed || r.State != RelayUploadUploading {
		t.Fatalf("meta update reset claim/state: %+v", r)
	}
	// A foreign key cannot claim.
	if err := s.ClaimRelayUpload("cl-0001", "k2"); err == nil {
		t.Fatal("foreign claim accepted")
	}

	// Terminal receipts are not claimable, and the claim stays recorded.
	row := RelayFile{ID: "f1", Size: 6}
	if err := s.FinishRelayUpload("cl-0001", RelayUploadCompleted, "ok", &row); err != nil {
		t.Fatal(err)
	}
	if err := s.ClaimRelayUpload("cl-0001", "k1"); !errors.Is(err, ErrReceiptTerminal) {
		t.Fatalf("terminal claim = %v, want ErrReceiptTerminal", err)
	}
	r, _ = s.GetRelayUpload("cl-0001")
	if r.State != RelayUploadCompleted || !r.Claimed {
		t.Fatalf("finished receipt = %+v", r)
	}
}

// TestRelayUploadClaimConcurrent proves the claim is atomic under concurrency:
// exactly one of many racers holds the claim.
func TestRelayUploadClaimConcurrent(t *testing.T) {
	s, err := Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	mustReserve(t, s, "cl-race", "k1")
	const n = 16
	var wg sync.WaitGroup
	results := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results <- s.ClaimRelayUpload("cl-race", "k1")
		}()
	}
	wg.Wait()
	close(results)
	won := 0
	for err := range results {
		switch {
		case err == nil:
			won++
		case errors.Is(err, ErrReceiptClaimed):
		default:
			t.Fatalf("unexpected claim error: %v", err)
		}
	}
	if won != 1 {
		t.Fatalf("claims won = %d, want exactly 1", won)
	}
}

// TestRelayUploadReserveConcurrent proves the reservation is atomic: exactly
// one of many concurrent same-id reservations wins.
func TestRelayUploadReserveConcurrent(t *testing.T) {
	s, err := Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	const id = "concurrent-id-1"
	const n = 16
	var wg sync.WaitGroup
	results := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results <- s.ReserveRelayUpload(id, "k1", "", "", 0)
		}()
	}
	wg.Wait()
	close(results)
	won := 0
	for err := range results {
		switch {
		case err == nil:
			won++
		case errors.Is(err, ErrUploadIDExists):
		default:
			t.Fatalf("unexpected reservation error: %v", err)
		}
	}
	if won != 1 {
		t.Fatalf("reservations won = %d, want exactly 1", won)
	}
}

// TestRelayUploadInterruptAndPrune covers the boot/maintenance sweeps.
func TestRelayUploadInterruptAndPrune(t *testing.T) {
	s, err := Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	mustReserve(t, s, "fresh", "k1")
	mustReserve(t, s, "stale", "k1")
	if err := s.Exec(`UPDATE relay_uploads SET updated_at=? WHERE id='stale'`,
		time.Now().Add(-48*time.Hour).UTC().Format(tsFormat)); err != nil {
		t.Fatal(err)
	}
	// Terminal receipt old enough to prune.
	mustReserve(t, s, "done", "k1")
	if err := s.FinishRelayUpload("done", RelayUploadCompleted, "ok", nil); err != nil {
		t.Fatal(err)
	}
	if err := s.Exec(`UPDATE relay_uploads SET updated_at=? WHERE id='done'`,
		time.Now().Add(-8*24*time.Hour).UTC().Format(tsFormat)); err != nil {
		t.Fatal(err)
	}

	// The maintenance cutoff only interrupts stale uploading receipts.
	if n, err := s.InterruptRelayUploadsBefore(time.Now().Add(-24*time.Hour), "stale interrupted"); err != nil || n != 1 {
		t.Fatalf("interrupt = %d err=%v", n, err)
	}
	r, _ := s.GetRelayUpload("fresh")
	if r.State != RelayUploadUploading {
		t.Fatalf("fresh receipt interrupted: %+v", r)
	}
	r, _ = s.GetRelayUpload("stale")
	if r.State != RelayUploadInterrupted || r.Message != "stale interrupted" {
		t.Fatalf("stale = %+v", r)
	}

	// The boot form (cutoff=now) interrupts everything still uploading.
	if n, err := s.InterruptRelayUploadsBefore(time.Now(), "restart"); err != nil || n != 1 {
		t.Fatalf("boot interrupt = %d err=%v", n, err)
	}
	r, _ = s.GetRelayUpload("fresh")
	if r.State != RelayUploadInterrupted {
		t.Fatalf("boot did not interrupt: %+v", r)
	}

	// Terminal receipts older than the retention window are pruned; the rest
	// stay.
	if n, err := s.PruneRelayUploads(time.Now().Add(-7 * 24 * time.Hour)); err != nil || n != 1 {
		t.Fatalf("prune = %d err=%v", n, err)
	}
	if _, err := s.GetRelayUpload("done"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("done survived prune: %v", err)
	}
	if _, err := s.GetRelayUpload("stale"); err != nil {
		t.Fatalf("recent receipt pruned: %v", err)
	}
}
