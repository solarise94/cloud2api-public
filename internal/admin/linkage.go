package admin

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"cloud2api/internal/config"
	"cloud2api/internal/openlist"
	"cloud2api/internal/store"
)

// unmanagedItem is one OpenList storage no cloud2api drive maps to (§8.2 1c).
// Storages are never exposed to any key until adopted — except a replacement
// candidate, which is only adoptable via the rebind confirmation.
type unmanagedItem struct {
	StorageID int64  `json:"storage_id"`
	MountPath string `json:"mount_path"`
	Driver    string `json:"driver"`
	Status    string `json:"status"`
	Disabled  bool   `json:"disabled"`
	// ReplacementFor names the drive whose recorded storage is gone while
	// this storage sits at its mount (a candidate awaiting confirmation).
	ReplacementFor string `json:"replacement_for,omitempty"`
}

// unmanagedItems is the ONE builder of the unmanaged list, shared by
// GET /admin/api/drives/unmanaged and POST /admin/api/drives/sync so the two
// listings can never disagree. Storages claimed by a drive's storage id are
// excluded, so are storages sitting at the mount of an id-less (static, not
// yet adopted) drive; a storage at the mount of a drive whose recorded id is
// absent from the snapshot is its replacement candidate and is listed with
// replacement_for set to that drive.
func unmanagedItems(drives []store.Drive, storages []openlist.StorageInfo) []unmanagedItem {
	inSnapshot := make(map[int64]bool, len(storages))
	for _, st := range storages {
		if st.ID != 0 {
			inSnapshot[st.ID] = true
		}
	}
	claimed := make(map[int64]bool, len(drives))
	mountOwner := make(map[string]store.Drive, len(drives))
	for _, d := range drives {
		if d.StorageID != 0 {
			claimed[d.StorageID] = true
		}
		mountOwner[d.MountPath] = d
	}
	items := []unmanagedItem{}
	for _, st := range storages {
		if st.ID == 0 || claimed[st.ID] {
			continue
		}
		if owner, ok := mountOwner[st.MountPath]; ok {
			if owner.StorageID == 0 {
				// A drive without a storage id still loosely owns the mount.
				continue
			}
			if !inSnapshot[owner.StorageID] {
				// The owner's recorded storage is gone: this storage is its
				// replacement candidate, confirmable only via rebind.
				items = append(items, unmanagedItem{
					StorageID: st.ID, MountPath: st.MountPath, Driver: st.Driver,
					Status: st.Status, Disabled: st.Disabled, ReplacementFor: owner.ID,
				})
			}
			// The mount belongs to a drive whose storage still exists.
			continue
		}
		items = append(items, unmanagedItem{
			StorageID: st.ID, MountPath: st.MountPath, Driver: st.Driver,
			Status: st.Status, Disabled: st.Disabled,
		})
	}
	return items
}

// handleListUnmanaged implements GET /admin/api/drives/unmanaged (§8.2 1c).
func (p *Portal) handleListUnmanaged(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), defaultAdminCallTimeout)
	defer cancel()
	storages, err := p.client.ListStorages(ctx)
	if err != nil {
		p.respondUpstreamError(w, err)
		return
	}
	drives, err := p.st.ListDrives()
	if err != nil {
		respondError(w, http.StatusInternalServerError, "store error")
		return
	}
	respondData(w, map[string]any{"storages": unmanagedItems(drives, storages)})
}

// claimedStorages returns the storage ids and mount paths currently mapped by
// drives.
func (p *Portal) claimedStorages() (map[int64]bool, map[string]bool, error) {
	drives, err := p.st.ListDrives()
	if err != nil {
		return nil, nil, err
	}
	ids := map[int64]bool{}
	mounts := map[string]bool{}
	for _, d := range drives {
		if d.StorageID != 0 {
			ids[d.StorageID] = true
		} else {
			mounts[d.MountPath] = true
		}
	}
	return ids, mounts, nil
}

// handleAdoptDrive implements POST /admin/api/drives/adopt {storage_id,id}
// (§刷新与纳管): record the drive ↔ storage mapping only. OpenList is never
// modified, no key gains access, and the response carries the storage's REAL
// state. A storage sitting at the mount of ANY existing drive (its drive's
// replacement candidate included) must be confirmed via rebind instead.
func (p *Portal) handleAdoptDrive(w http.ResponseWriter, r *http.Request) {
	var body struct {
		StorageID int64  `json:"storage_id"`
		ID        string `json:"id"`
	}
	if err := decodeBody(w, r, &body); err != nil {
		respondError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if body.StorageID == 0 {
		respondError(w, http.StatusBadRequest, "storage_id is required")
		return
	}
	if body.ID == "" {
		respondError(w, http.StatusBadRequest, "id is required")
		return
	}
	if err := config.ValidateAccountID(body.ID); err != nil {
		respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	if existing, err := p.st.GetDrive(body.ID); err == nil && existing.ID == body.ID {
		respondError(w, http.StatusConflict, "drive "+body.ID+" already exists")
		return
	} else if err != nil && !errors.Is(err, store.ErrNotFound) {
		respondError(w, http.StatusInternalServerError, "store error")
		return
	}

	// adopt and rebind serialize on one mutex: two concurrent calls must not
	// both claim the same storage.
	p.claimMu.Lock()
	defer p.claimMu.Unlock()

	ctx, cancel := context.WithTimeout(r.Context(), defaultAdminCallTimeout)
	defer cancel()
	storages, err := p.client.ListStorages(ctx)
	if err != nil {
		p.respondUpstreamError(w, err)
		return
	}
	var target *storage
	for i := range storages {
		if storages[i].ID == body.StorageID {
			target = &storages[i]
			break
		}
	}
	if target == nil {
		respondError(w, http.StatusNotFound, "未找到该 OpenList 存储（可能已被删除或纳管）；请刷新后重试")
		return
	}
	claimedIDs, _, err := p.claimedStorages()
	if err != nil {
		respondError(w, http.StatusInternalServerError, "store error")
		return
	}
	if claimedIDs[target.ID] {
		respondError(w, http.StatusConflict, "storage "+strconv.FormatInt(target.ID, 10)+" ("+target.MountPath+") is already managed by a drive")
		return
	}
	// The target's mount must not belong to ANY existing drive (store mount
	// or registry mount) — including the replacement candidate of a drive
	// whose recorded storage was deleted.
	drives, err := p.st.ListDrives()
	if err != nil {
		respondError(w, http.StatusInternalServerError, "store error")
		return
	}
	inSnapshot := map[int64]bool{}
	for _, st := range storages {
		if st.ID != 0 {
			inSnapshot[st.ID] = true
		}
	}
	for _, d := range drives {
		if d.MountPath != target.MountPath {
			continue
		}
		if d.StorageID != 0 && !inSnapshot[d.StorageID] {
			respondError(w, http.StatusConflict, "存储 #"+strconv.FormatInt(target.ID, 10)+" 的挂载点属于网盘 "+d.ID+"（其原存储已被删除）；请在该网盘上通过「确认使用替代存储」完成重绑")
			return
		}
		respondError(w, http.StatusConflict, "存储 #"+strconv.FormatInt(target.ID, 10)+" 的挂载点 "+target.MountPath+" 已被网盘 "+d.ID+" 占用")
		return
	}
	if p.registry.HasMount(target.MountPath) {
		respondError(w, http.StatusConflict, "存储 #"+strconv.FormatInt(target.ID, 10)+" 的挂载点 "+target.MountPath+" 已被其他网盘占用；请刷新后重试")
		return
	}

	provider := ProviderForDriver(target.Driver)
	stored := store.Drive{
		ID:        body.ID,
		Provider:  provider,
		Driver:    target.Driver,
		StorageID: target.ID,
		MountPath: target.MountPath,
		Enabled:   true,
		Static:    false,
		CreatedAt: time.Now().UTC(),
	}
	if err := p.registry.Add(config.Account{
		ID: stored.ID, Provider: stored.Provider, MountPath: stored.MountPath, Enabled: true,
	}); err != nil {
		respondError(w, http.StatusConflict, err.Error())
		return
	}
	if err := p.st.InsertDrive(stored); err != nil {
		_ = p.registry.Remove(stored.ID)
		p.log.Error("drive store insert failed", "id", body.ID, "err", err)
		respondError(w, http.StatusInternalServerError, "drive registered but persisting it failed")
		return
	}
	if err := p.st.InsertEvent(body.ID, "adopted",
		"adopted OpenList storage "+strconv.FormatInt(target.ID, 10)+" (driver "+target.Driver+", mount "+target.MountPath+"); OpenList configuration untouched"); err != nil {
		p.log.Warn("event insert failed", "err", err)
	}
	p.InvalidateStorageCache()

	// The row carries the storage's REAL state (异常或停用的存储也允许纳管).
	byMount, byID := indexStorages(storages)
	respondData(w, p.driveRowFromDrive(stored, byMount, byID, p.openlistBaseURL()))
}

// handleRebindDrive implements POST /admin/api/drives/{id}/rebind
// {storage_id} (§存储被删除与替代存储): an administrator confirms the
// replacement storage sitting at a drive's mount. The mapping moves to the new
// storage id — from that moment the drive's existing key grants apply to it —
// and the storage's real driver is recorded.
func (p *Portal) handleRebindDrive(w http.ResponseWriter, r *http.Request, id string) {
	var body struct {
		StorageID int64 `json:"storage_id"`
	}
	if err := decodeBody(w, r, &body); err != nil {
		respondError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if body.StorageID == 0 {
		respondError(w, http.StatusBadRequest, "storage_id is required")
		return
	}

	// Serialize with adopt: two concurrent claims of one storage must not
	// both succeed.
	p.claimMu.Lock()
	defer p.claimMu.Unlock()

	ctx, cancel := context.WithTimeout(r.Context(), defaultAdminCallTimeout)
	defer cancel()
	storages, err := p.client.ListStorages(ctx)
	if err != nil {
		respondError(w, http.StatusBadGateway, "OpenList 存储列表读取失败，无法确认替代存储："+err.Error())
		return
	}
	d, err := p.driveByID(id)
	if errors.Is(err, store.ErrNotFound) {
		respondError(w, http.StatusNotFound, "drive not found")
		return
	} else if err != nil {
		respondError(w, http.StatusInternalServerError, "store error")
		return
	}
	byMount, byID := indexStorages(storages)
	// The recorded storage must actually be gone.
	if d.StorageID != 0 {
		if _, still := byID[d.StorageID]; still {
			respondError(w, http.StatusConflict, "原存储 "+strconv.FormatInt(d.StorageID, 10)+" 仍存在；请先「刷新并纳管」确认状态后再操作")
			return
		}
	}
	cand, ok := byID[body.StorageID]
	if !ok {
		respondError(w, http.StatusNotFound, "未找到该 OpenList 存储（可能已被删除或纳管）；请刷新后重试")
		return
	}
	// The candidate must sit at the drive's mount (the registry mount is the
	// one the data plane addresses).
	mount := d.MountPath
	if a, ok := p.registry.Get(id); ok && a.MountPath != "" {
		mount = a.MountPath
	}
	if cand.MountPath != mount {
		respondError(w, http.StatusConflict, "存储 #"+strconv.FormatInt(cand.ID, 10)+" 已不在挂载点 "+mount+" 上（当前在 "+cand.MountPath+"）；请刷新后重试")
		return
	}
	drives, err := p.st.ListDrives()
	if err != nil {
		respondError(w, http.StatusInternalServerError, "store error")
		return
	}
	for _, other := range drives {
		if other.ID == d.ID {
			continue
		}
		if other.StorageID != 0 && other.StorageID == cand.ID {
			respondError(w, http.StatusConflict, "存储 #"+strconv.FormatInt(cand.ID, 10)+" 已被网盘 "+other.ID+" 纳管；请刷新后重试")
			return
		}
		if other.StorageID == 0 && other.MountPath == mount {
			respondError(w, http.StatusConflict, "挂载点 "+mount+" 已被网盘 "+other.ID+" 占用；请刷新后重试")
			return
		}
	}

	oldID, oldDriver := d.StorageID, d.Driver
	d.StorageID = cand.ID
	d.Driver = cand.Driver
	if err := p.st.SetDriveDriver(id, cand.Driver); err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			respondError(w, http.StatusInternalServerError, "store error")
			return
		}
		// A static registry-only drive (no store row yet): materialize it.
		if err := p.st.InsertDrive(d); err != nil {
			respondError(w, http.StatusInternalServerError, "store error")
			return
		}
	} else if err := p.st.ApplyReconcile(id, "", d.MountSeen, nil, &d.StorageID); err != nil {
		// The id change also clears the persisted replacement_found, which
		// Resolve would otherwise keep showing until the next reconcile.
		respondError(w, http.StatusInternalServerError, "store error")
		return
	}
	if err := p.st.InsertEvent(id, "storage_rebound",
		"administrator confirmed the replacement storage: rebound from storage "+strconv.FormatInt(oldID, 10)+
			" (driver "+oldDriver+") to storage "+strconv.FormatInt(cand.ID, 10)+" (driver "+cand.Driver+
			") at mount "+mount+"; existing key grants now apply to the new storage"); err != nil {
		p.log.Warn("event insert failed", "err", err)
	}
	// Let the state settle from the same snapshot (healthy/error/...).
	if p.reconciler != nil {
		if rerr := p.reconciler.ReconcileWith(ctx, storages); rerr != nil {
			p.log.Warn("rebind reconcile failed", "drive", id, "err", rerr)
		}
	}
	p.InvalidateStorageCache()
	fresh, err := p.st.GetDrive(id)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "store error")
		return
	}
	respondData(w, p.driveRowFromDrive(fresh, byMount, byID, p.openlistBaseURL()))
}

// handleEvents implements GET /admin/api/events?limit= (§8.2 1c).
func (p *Portal) handleEvents(w http.ResponseWriter, r *http.Request) {
	limit := 100
	if raw := r.URL.Query().Get("limit"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			limit = n
		}
	}
	events, err := p.st.ListEvents(limit)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "store error")
		return
	}
	rows := make([]map[string]any, 0, len(events))
	for _, e := range events {
		rows = append(rows, map[string]any{
			"ts":     e.TS.UTC().Format(time.RFC3339Nano),
			"drive":  e.Drive,
			"kind":   e.Kind,
			"detail": e.Detail,
		})
	}
	respondData(w, map[string]any{"events": rows})
}
