package agentapi

import (
	"context"
	"time"

	"cloud2api/internal/gateway"
)

// ---- trash endpoints (§4.10.3) ----

func (s *Server) handleTrashList(c *call) {
	items, err := s.st.ListTrashForKey(c.key.ID)
	if err != nil {
		c.fail(errUpstream("trash listing failed: " + err.Error()))
		return
	}
	out := make([]map[string]any, 0, len(items))
	for _, t := range items {
		out = append(out, map[string]any{
			"id":          t.ID,
			"drive":       t.Drive,
			"path":        t.OrigPath,
			"is_dir":      t.IsDir,
			"size":        t.Size,
			"deleted_at":  t.DeletedAt.UTC().Format(time.RFC3339),
			"purge_after": t.PurgeAfter.UTC().Format(time.RFC3339),
		})
	}
	c.ok(map[string]any{"items": out})
}

// handleTrashRestore moves a trashed item back to its original path.
func (s *Server) handleTrashRestore(c *call) {
	var body struct {
		ID int64 `json:"id"`
	}
	if !decodeBody(c, &body) {
		return
	}
	if body.ID == 0 {
		c.fail(errInvalidRequest("field `id` is required"))
		return
	}
	item, err := s.st.GetTrashItem(body.ID, c.key.ID)
	if err != nil {
		c.fail(errPathNotFound("trash item").hint("list the trash with GET /v1/trash"))
		return
	}
	c.drive = item.Drive
	// The restore needs delete rights on the drive (it undoes a delete).
	grant, ok := c.key.DriveGrant(item.Drive)
	if !ok {
		c.fail(errForbidden("drive \"" + item.Drive + "\" is no longer granted to this key"))
		return
	}
	if !c.key.CanOn(item.Drive, "delete") {
		c.fail(errForbidden("missing \"delete\" permission on drive \"" + item.Drive + "\""))
		return
	}
	if acct, known := s.registry.Get(item.Drive); !known {
		c.fail(errDriveNotFound(item.Drive, nil))
		return
	} else if !acct.Enabled {
		c.fail(errDriveUnavailable(item.Drive, "the drive is disabled"))
		return
	}
	// Restore moves bytes on the drive: the storage guard must allow it
	// first (a replaced storage must not receive restored files).
	if verdict := s.guard.Check(c.r.Context(), item.Drive); !verdict.Allowed() {
		c.fail(driveUnavailableErr(item.Drive, verdict.Message))
		return
	}
	c.root = grant.Root
	c.setPath(item.OrigPath)

	ctx, cancel := context.WithTimeout(c.r.Context(), fsTimeout)
	defer cancel()

	// The original parent must exist again. origInternal carries the mount
	// prefix; all upstream calls below use internal paths.
	origFull := joinRoot(grant.Root, item.OrigPath)
	origInternal := s.toInternal(item.Drive, origFull)
	origDirInternal, oBase := parentOf(origInternal)
	if origDirInternal != "/" {
		if err := s.mkdirAll(ctx, origDirInternal); err != nil {
			c.fail(fsError(err))
			return
		}
	}
	// Target must be free.
	if _, err := s.fsGet(ctx, origInternal); err == nil {
		c.fail(errAlreadyExists(item.OrigPath).hint("move or delete the file currently at that path first"))
		return
	}
	trashInternal := s.toInternal(item.Drive, item.TrashPath)
	if _, err := s.fsGet(ctx, trashInternal); err != nil {
		// Already gone from the trash: drop the row (idempotent restore).
		_ = s.st.DeleteTrashItem(item.ID)
		c.fail(errPathNotFound(item.TrashPath).hint("the trash entry no longer exists on the drive"))
		return
	}
	if err := s.moveAcross(ctx, trashInternal, origDirInternal, oBase); err != nil {
		c.fail(fsError(err))
		return
	}
	_ = s.st.DeleteTrashItem(item.ID)
	c.ok(map[string]any{"id": item.ID, "path": item.OrigPath, "restored": true})
}

// handleTrashPurge removes a trashed item permanently (purge permission).
func (s *Server) handleTrashPurge(c *call) {
	var body struct {
		ID int64 `json:"id"`
	}
	if !decodeBody(c, &body) {
		return
	}
	if body.ID == 0 {
		c.fail(errInvalidRequest("field `id` is required"))
		return
	}
	item, err := s.st.GetTrashItem(body.ID, c.key.ID)
	if err != nil {
		c.fail(errPathNotFound("trash item").hint("list the trash with GET /v1/trash"))
		return
	}
	c.drive = item.Drive
	if !c.key.CanOn(item.Drive, "purge") {
		c.fail(errForbidden("missing \"purge\" permission on drive \"" + item.Drive + "\""))
		return
	}
	c.setPath(item.OrigPath)

	// Purge deletes bytes on the drive: guard first (a replaced storage must
	// not lose files it never held).
	if verdict := s.guard.Check(c.r.Context(), item.Drive); !verdict.Allowed() {
		c.fail(driveUnavailableErr(item.Drive, verdict.Message))
		return
	}

	ctx, cancel := context.WithTimeout(c.r.Context(), fsTimeout)
	defer cancel()
	internal := s.toInternal(item.Drive, item.TrashPath)
	if _, err := s.fsGet(ctx, internal); err == nil {
		dir, base := parentOf(internal)
		if err := s.fsRemove(ctx, dir, []string{base}); err != nil {
			c.fail(fsError(err))
			return
		}
	} else if !isNotFoundError(err) {
		c.fail(fsError(err))
		return
	}
	_ = s.st.DeleteTrashItem(item.ID)
	c.ok(map[string]any{"id": item.ID, "purged": true})
}

// toInternal maps a full drive path to the OpenList path for drive.
func (s *Server) toInternal(drive, full string) string {
	acct, ok := s.registry.Get(drive)
	if !ok {
		return full
	}
	internal, err := gateway.MountJoin(acct.MountPath, full)
	if err != nil {
		return full
	}
	return internal
}

// StartMaintenance launches the background loop: purge expired trash items,
// prune the audit log (30 days) and the idempotency cache (24h).
func (s *Server) StartMaintenance(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(10 * time.Minute)
		defer ticker.Stop()
		s.runMaintenance(ctx) // run once at startup
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.runMaintenance(ctx)
			}
		}
	}()
}

// runMaintenance purges expired trash items, prunes the audit log (30 days)
// and the idempotency cache (24h). It returns how many trash items were
// purged (the admin maintenance report surfaces it).
func (s *Server) runMaintenance(ctx context.Context) int {
	purged := 0
	items, err := s.st.ExpiredTrash(time.Now())
	if err != nil {
		s.log.Warn("trash expiry query failed", "err", err)
	}
	for _, t := range items {
		cctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
		internal := s.toInternal(t.Drive, t.TrashPath)
		if _, err := s.fsGet(cctx, internal); err == nil {
			dir, base := parentOf(internal)
			if err := s.fsRemove(cctx, dir, []string{base}); err != nil {
				s.log.Warn("trash purge failed", "id", t.ID, "err", err)
				cancel()
				continue
			}
		}
		cancel()
		_ = s.st.DeleteTrashItem(t.ID)
		purged++
		s.log.Info("purged expired trash item", "id", t.ID, "drive", t.Drive, "path", t.OrigPath)
	}
	if err := s.st.PruneAudit(30 * 24 * time.Hour); err != nil {
		s.log.Warn("audit prune failed", "err", err)
	}
	if err := s.st.PruneIdempotency(idempotencyTTL); err != nil {
		s.log.Warn("idempotency prune failed", "err", err)
	}
	return purged
}
