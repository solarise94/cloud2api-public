package admin

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"cloud2api/internal/openlist"
	"cloud2api/internal/reconcile"
	"cloud2api/internal/storageid"
	"cloud2api/internal/store"
)

// driveRow is the §8.2 GET /admin/api/drives row.
type driveRow struct {
	ID        string `json:"id"`
	Provider  string `json:"provider"`
	Driver    string `json:"driver"`
	MountPath string `json:"mount_path"`
	StorageID int64  `json:"storage_id"`
	// Enabled is cloud2api's own switch: whether the drive is open to /v1 and
	// the Gokapi layer at all. Toggling it never touches OpenList.
	Enabled bool   `json:"enabled"`
	State   string `json:"state"`
	// StatusText is the raw OpenList status of the matched storage.
	StatusText string `json:"status_text"`
	// StorageDisabled is true when the matched OpenList storage is disabled in
	// the snapshot; false when no storage is matched.
	StorageDisabled bool   `json:"storage_disabled"`
	KeyCount        int    `json:"key_count"`
	CreatedAt       string `json:"created_at"`
	Static          bool   `json:"static"`
	// OpenlistEditURL links into the OpenList storage editor; only set while
	// the drive currently matches a storage and openlist_public_url is valid.
	OpenlistEditURL string `json:"openlist_edit_url"`
	// OpenlistReauthURL links into the patched OpenList's login page to
	// re-authorize the matched storage (IMPLEMENTATION-openlist-login-fork
	// §适配补丁范围: cloud2api links to the page, it never sees credentials).
	// Set only when the login module answered the probe, the base URL is
	// valid, the drive matches a storage and some platform re-authorizes that
	// storage's driver; the native editor above stays the advanced fallback.
	OpenlistReauthURL string `json:"openlist_reauth_url"`
	// Replacement names the unconfirmed candidate storage sitting at the
	// drive's mount while its recorded storage is gone; null otherwise.
	Replacement *replacementInfo `json:"replacement"`
}

// replacementInfo describes a replacement candidate (IMPLEMENTATION
// §存储被删除与替代存储): an administrator must confirm it (rebind) before
// agents may access it.
type replacementInfo struct {
	StorageID     int64  `json:"storage_id"`
	Driver        string `json:"driver"`
	MountPath     string `json:"mount_path"`
	Status        string `json:"status"`
	Disabled      bool   `json:"disabled"`
	DriverChanged bool   `json:"driver_changed"`
}

// accountRow keeps the v0.3 GET /admin/api/accounts shape (acceptance C).
type accountRow struct {
	ID              string `json:"id"`
	Provider        string `json:"provider"`
	MountPath       string `json:"mount_path"`
	Enabled         bool   `json:"enabled"`
	Dynamic         bool   `json:"dynamic"`
	StorageID       int64  `json:"storage_id,omitempty"`
	CreatedAt       string `json:"created_at,omitempty"`
	OpenlistStatus  string `json:"openlist_status,omitempty"`
	OpenlistEnabled bool   `json:"openlist_enabled"`
}

// indexStorages builds the by-mount / by-id lookup maps of one snapshot.
func indexStorages(storages []openlist.StorageInfo) (byMount map[string]openlist.StorageInfo, byID map[int64]openlist.StorageInfo) {
	byMount = make(map[string]openlist.StorageInfo, len(storages))
	byID = make(map[int64]openlist.StorageInfo, len(storages))
	for _, st := range storages {
		byMount[st.MountPath] = st
		if st.ID != 0 {
			byID[st.ID] = st
		}
	}
	return byMount, byID
}

// toStorageIDDrive maps a store drive row onto the identity subset
// storageid.Resolve classifies.
func toStorageIDDrive(d store.Drive) storageid.Drive {
	return storageid.Drive{
		StorageID: d.StorageID, MountPath: d.MountPath,
		Static: d.Static, Enabled: d.Enabled, State: reconcile.DisplayState(d.State),
	}
}

// driveRowFromDrive builds the §8.2 row of one drive against a snapshot
// (state vocabulary and replacement detection come from storageid.Resolve —
// the mount fallback is reserved for drives that never recorded an id).
func (p *Portal) driveRowFromDrive(d store.Drive, byMount map[string]openlist.StorageInfo, byID map[int64]openlist.StorageInfo, base string) driveRow {
	res := storageid.Resolve(toStorageIDDrive(d), byMount, byID)
	driver := d.Driver
	if driver == "" {
		if dn, ok := DriverFor(d.Provider); ok {
			driver = dn
		}
	}
	storageID := d.StorageID
	mount := d.MountPath
	if res.Storage != nil {
		storageID = res.Storage.ID
		if res.Storage.MountPath != "" {
			mount = res.Storage.MountPath
		}
		// The OpenList storage is the source of truth for the driver
		// (Phase 2 fix): config.yaml-era rows guessed it from the
		// provider template, which is wrong for simulated netdisks.
		if res.Storage.Driver != "" {
			driver = res.Storage.Driver
		}
	}
	state, statusText := res.State, res.StatusText
	switch {
	case state == storageid.StateOrphaned && statusText == "":
		statusText = "no OpenList storage for this drive"
	case state == storageid.StateReplacementFound:
		statusText = fmt.Sprintf("storage %d not found", d.StorageID)
		if res.Replacement != nil {
			statusText = fmt.Sprintf("storage %d not found; unmanaged storage %d found on mount %s",
				d.StorageID, res.Replacement.ID, d.MountPath)
		}
	}
	var repl *replacementInfo
	if res.Replacement != nil {
		repl = &replacementInfo{
			StorageID:     res.Replacement.ID,
			Driver:        res.Replacement.Driver,
			MountPath:     res.Replacement.MountPath,
			Status:        res.Replacement.Status,
			Disabled:      res.Replacement.Disabled,
			DriverChanged: res.Replacement.Driver != driver,
		}
	}
	editURL := ""
	reauthURL := ""
	storageDisabled := false
	// The reauth link reflects the last login-module probe (openlist-patch):
	// listings that just probed see their fresh result, adopt/rebind rows
	// reuse whatever recent probe exists.
	connect, connectOK := p.connectSnapshot()
	if st, ok := byID[storageID]; ok {
		// storage_disabled and the edit link reflect the OpenList snapshot
		// alone, independent of cloud2api's own enabled switch.
		storageDisabled = st.Disabled
		editURL = openlistStorageEditURL(base, storageID)
		if connectOK && connect.SupportsDriver(st.Driver) {
			reauthURL = openlistReauthURL(base, storageID)
		}
	}
	keyCount, _ := p.st.DriveKeyCount(d.ID)
	return driveRow{
		ID:                d.ID,
		Provider:          d.Provider,
		Driver:            driver,
		MountPath:         mount,
		StorageID:         storageID,
		Enabled:           d.Enabled,
		State:             state,
		StatusText:        statusText,
		StorageDisabled:   storageDisabled,
		KeyCount:          keyCount,
		CreatedAt:         d.CreatedAt.UTC().Format(time.RFC3339),
		Static:            d.Static,
		OpenlistEditURL:   editURL,
		OpenlistReauthURL: reauthURL,
		Replacement:       repl,
	}
}

// driveRowsFromSnapshot builds the rows of every drive against one snapshot.
func (p *Portal) driveRowsFromSnapshot(drives []store.Drive, storages []openlist.StorageInfo) []driveRow {
	byMount, byID := indexStorages(storages)
	base := p.openlistBaseURL()
	rows := make([]driveRow, 0, len(drives))
	for _, d := range drives {
		rows = append(rows, p.driveRowFromDrive(d, byMount, byID, base))
	}
	return rows
}

func (p *Portal) driveRows(ctx context.Context) ([]driveRow, error) {
	storages, err := p.cachedStorages(ctx)
	if err != nil {
		return nil, err
	}
	drives, err := p.st.ListDrives()
	if err != nil {
		return nil, err
	}
	return p.driveRowsFromSnapshot(drives, storages), nil
}

// refreshSnapshot reads ONE fresh paged OpenList snapshot, reconciles on it
// (when a reconciler is wired) and stores it as the portal storage cache. When
// the fresh read fails the cached snapshot is returned together with the
// failure text (IMPLEMENTATION §刷新与纳管: the listing degrades to the cache
// with snapshot_error); both returns are empty only when no read has ever
// succeeded.
func (p *Portal) refreshSnapshot(ctx context.Context) ([]openlist.StorageInfo, string) {
	storages, err := p.client.ListStorages(ctx)
	if err != nil {
		p.storageMu.Lock()
		cached := p.storageSnap
		p.storageMu.Unlock()
		if cached != nil {
			return cached, "OpenList 存储列表读取失败（" + err.Error() + "）：以下为最近一次成功读取的缓存数据"
		}
		return nil, err.Error()
	}
	if p.reconciler != nil {
		if rerr := p.reconciler.ReconcileWith(ctx, storages); rerr != nil {
			p.log.Warn("portal-list reconcile failed", "err", rerr)
		}
	}
	if storages == nil {
		// An empty but successful read is a valid snapshot, not "no data".
		storages = []openlist.StorageInfo{}
	}
	p.storeSnapshotCache(storages)
	return storages, ""
}

func (p *Portal) handleListDrives(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), defaultAdminCallTimeout)
	defer cancel()
	storages, snapshotErr := p.refreshSnapshot(ctx)
	if storages == nil {
		respondError(w, http.StatusBadGateway, "OpenList 存储列表读取失败（"+snapshotErr+"），且没有可用的缓存数据")
		return
	}
	drives, err := p.st.ListDrives()
	if err != nil {
		respondError(w, http.StatusInternalServerError, "store error")
		return
	}
	// The login-module probe feeds the rows' reauth links; it can only fail
	// itself — an absent module or a failed probe just leaves the links off.
	p.connectState(ctx, false)
	out := map[string]any{"drives": p.driveRowsFromSnapshot(drives, storages)}
	if snapshotErr != "" {
		out["snapshot_error"] = snapshotErr
	}
	respondData(w, out)
}

// handleSyncDrives implements POST /admin/api/drives/sync (刷新并纳管): ONE
// paged snapshot drives the whole refresh — reconcile runs on it, and both the
// managed rows and the unmanaged list are built from that same slice, so the
// two listings can never disagree. A read or reconcile failure answers 502 and
// refreshes nothing. One login-module probe is forced per sync and its result
// feeds the returned rows' reauth links (刷新后重新授权, openlist-patch).
func (p *Portal) handleSyncDrives(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), defaultAdminCallTimeout)
	defer cancel()
	storages, err := p.client.ListStorages(ctx)
	if err != nil {
		respondError(w, http.StatusBadGateway, "OpenList 存储列表读取失败，未刷新："+err.Error())
		return
	}
	if p.reconciler != nil {
		if err := p.reconciler.ReconcileWith(ctx, storages); err != nil {
			respondError(w, http.StatusBadGateway, "刷新未完成：对账失败（"+err.Error()+"）")
			return
		}
	}
	p.storeSnapshotCache(storages)
	// One forced probe per sync (past the TTL) feeds the rows below; a failed
	// probe only drops the reauth links, never the sync result.
	p.connectState(ctx, true)
	drives, err := p.st.ListDrives()
	if err != nil {
		respondError(w, http.StatusInternalServerError, "store error")
		return
	}
	respondData(w, map[string]any{
		"drives":    p.driveRowsFromSnapshot(drives, storages),
		"unmanaged": unmanagedItems(drives, storages),
		"synced_at": time.Now().UTC().Format(time.RFC3339),
	})
}

// handleListAccounts serves the legacy alias with the v0.3 shape.
func (p *Portal) handleListAccounts(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), defaultAdminCallTimeout)
	defer cancel()
	rows, err := p.driveRows(ctx)
	if err != nil {
		p.respondUpstreamError(w, err)
		return
	}
	accounts := make([]accountRow, 0, len(rows))
	for _, d := range rows {
		accounts = append(accounts, accountRow{
			ID:              d.ID,
			Provider:        d.Provider,
			MountPath:       d.MountPath,
			Enabled:         d.State != "disabled",
			Dynamic:         !d.Static,
			StorageID:       d.StorageID,
			CreatedAt:       d.CreatedAt,
			OpenlistStatus:  statusForAccount(d),
			OpenlistEnabled: d.State != "disabled",
		})
	}
	respondData(w, map[string]any{"accounts": accounts})
}

func statusForAccount(d driveRow) string {
	if d.State == "healthy" {
		return "work"
	}
	if d.State == "orphaned" {
		return "storage missing"
	}
	if d.State == storageid.StateReplacementFound {
		return "storage replaced; confirmation pending"
	}
	return d.StatusText
}

// serveDriveItem routes /admin/api/{drives|accounts}/{id}[...] plus the
// linkage endpoints under /admin/api/drives/ (unmanaged, adopt, sync, rebind).
func (p *Portal) serveDriveItem(w http.ResponseWriter, r *http.Request, pathName string) {
	var rest string
	if strings.HasPrefix(pathName, "/admin/api/drives/") {
		rest = strings.TrimPrefix(pathName, "/admin/api/drives/")
	} else {
		rest = strings.TrimPrefix(pathName, "/admin/api/accounts/")
	}
	switch {
	case rest == "unmanaged" && r.Method == http.MethodGet && strings.HasPrefix(pathName, "/admin/api/drives/"):
		p.handleListUnmanaged(w, r)
	case rest == "adopt" && r.Method == http.MethodPost && strings.HasPrefix(pathName, "/admin/api/drives/"):
		p.handleAdoptDrive(w, r)
	case rest == "sync" && r.Method == http.MethodPost && strings.HasPrefix(pathName, "/admin/api/drives/"):
		p.handleSyncDrives(w, r)
	case strings.HasSuffix(rest, "/rebind") && r.Method == http.MethodPost && strings.HasPrefix(pathName, "/admin/api/drives/"):
		id := strings.TrimSuffix(rest, "/rebind")
		if id == "" || strings.Contains(id, "/") {
			respondError(w, http.StatusNotFound, "not found")
			return
		}
		p.handleRebindDrive(w, r, id)
	case strings.HasSuffix(rest, "/enable"), strings.HasSuffix(rest, "/disable"):
		enable := strings.HasSuffix(rest, "/enable")
		id := strings.TrimSuffix(strings.TrimSuffix(rest, "/enable"), "/disable")
		if r.Method != http.MethodPost {
			respondMethodNotAllowed(w, http.MethodPost)
			return
		}
		p.handleSetDriveEnabled(w, r, id, enable)
	case !strings.Contains(rest, "/") && r.Method == http.MethodDelete:
		p.handleDeleteDrive(w, r, rest)
	default:
		respondError(w, http.StatusNotFound, "not found")
	}
}

func (p *Portal) driveByID(id string) (store.Drive, error) {
	d, err := p.st.GetDrive(id)
	if errors.Is(err, store.ErrNotFound) {
		// A static config account may not be in the store yet (fresh DB);
		// fall back to the registry.
		if acct, ok := p.registry.Get(id); ok && p.registry.Static(id) {
			return store.Drive{
				ID: acct.ID, Provider: acct.Provider, MountPath: acct.MountPath,
				Enabled: acct.Enabled, Static: true,
			}, nil
		}
	}
	return d, err
}

// handleSetDriveEnabled implements POST /admin/api/{drives|accounts}/{id}/
// {enable,disable} (IMPLEMENTATION §已纳管网盘): it flips ONLY cloud2api's own
// switch — whether the drive is open to /v1 and the Gokapi layer. OpenList is
// never called; a storage disabled over there shows up via reconcile as
// storage_disabled. The persisted drive state is made consistent immediately
// and the refreshed drive row is returned.
func (p *Portal) handleSetDriveEnabled(w http.ResponseWriter, r *http.Request, id string, enable bool) {
	d, err := p.driveByID(id)
	if errors.Is(err, store.ErrNotFound) {
		respondError(w, http.StatusNotFound, "drive not found")
		return
	} else if err != nil {
		respondError(w, http.StatusInternalServerError, "store error")
		return
	}
	if d.Static {
		respondError(w, http.StatusConflict, "static drives (config.yaml) cannot be enabled or disabled via the portal")
		return
	}
	if err := p.st.SetDriveEnabled(id, enable); err != nil {
		p.log.Error("drive store update failed after enable/disable", "id", id, "err", err)
		respondError(w, http.StatusInternalServerError, "store error")
		return
	}
	if err := p.registry.SetEnabled(id, enable); err != nil {
		p.log.Error("registry update failed after enable/disable", "id", id, "err", err)
	}
	// Event kinds the frontend labels (ui.js EVENT_KIND): cloud2api's own
	// switch only — the OpenList storage is untouched.
	kind, text := "enabled", "drive enabled in cloud2api (open to /v1 and the Gokapi layer); the OpenList storage was not modified"
	if !enable {
		kind, text = "disabled", "drive disabled in cloud2api (closed to /v1 and the Gokapi layer); the OpenList storage stays as it is"
	}
	if err := p.st.InsertEvent(id, kind, text); err != nil {
		p.log.Warn("event insert failed", "err", err)
	}

	ctx, cancel := context.WithTimeout(r.Context(), defaultAdminCallTimeout)
	defer cancel()
	// Make the persisted drive state consistent right away so a stale
	// disabled_local never survives the switch: with a reconciler installed
	// run it on one fresh snapshot, otherwise recompute this drive's state
	// from the same snapshot by hand.
	persist := ""
	if !enable {
		persist = reconcile.StateDisabledLoc
	}
	storages, serr := p.client.ListStorages(ctx)
	switch {
	case serr != nil && enable:
		// OpenList unreadable: clear the state and let the next reconcile
		// pass recompute it.
		persist = ""
	case serr != nil:
		// Disable with OpenList unreadable: disabled_local stands.
	case p.reconciler != nil:
		if rerr := p.reconciler.ReconcileWith(ctx, storages); rerr != nil {
			p.log.Warn("reconcile after enable/disable failed", "id", id, "err", rerr)
		}
	case enable:
		byMount, byID := indexStorages(storages)
		d.Enabled = true
		res := storageid.Resolve(toStorageIDDrive(d), byMount, byID)
		if res.State == storageid.StateDisabled {
			persist = reconcile.StateDisabledOL
		} else {
			persist = res.State
		}
	}
	if p.reconciler == nil || serr != nil {
		if err := p.st.ApplyReconcile(id, persist, d.MountSeen, nil, nil); err != nil {
			p.log.Warn("drive state persist failed after enable/disable", "id", id, "err", err)
		}
	}

	fresh, err := p.st.GetDrive(id)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "store error")
		return
	}
	// The returned row's reauth link needs the login-module state; the probe
	// must never fail the switch itself.
	p.connectState(ctx, false)
	if serr == nil {
		// The fresh snapshot also refreshes the portal storage cache.
		p.storeSnapshotCache(storages)
		byMount, byID := indexStorages(storages)
		respondData(w, p.driveRowFromDrive(fresh, byMount, byID, p.openlistBaseURL()))
		return
	}
	p.InvalidateStorageCache()
	cached, cerr := p.cachedStorages(ctx)
	byMount, byID := indexStorages(cached)
	row := p.driveRowFromDrive(fresh, byMount, byID, p.openlistBaseURL())
	if cerr != nil {
		// No snapshot: derive the display state from what was just persisted.
		row.State = reconcile.DisplayState(persist)
		row.StatusText = ""
	}
	respondData(w, row)
}

// handleDeleteDrive implements DELETE /admin/api/{drives|accounts}/{id}
// (IMPLEMENTATION §已纳管网盘: 移出): it removes ONLY the drive ↔ storage
// mapping. The keep_storage query parameter is accepted for compatibility and
// ignored — the OpenList storage is never resolved or touched, and deleting
// the storage itself is done in the OpenList admin UI. A "released" event is
// recorded every time; afterwards the storage shows up in the unmanaged list.
func (p *Portal) handleDeleteDrive(w http.ResponseWriter, r *http.Request, id string) {
	d, err := p.driveByID(id)
	if errors.Is(err, store.ErrNotFound) {
		respondError(w, http.StatusNotFound, "drive not found")
		return
	} else if err != nil {
		respondError(w, http.StatusInternalServerError, "store error")
		return
	}
	if d.Static {
		respondError(w, http.StatusConflict, "static drives (config.yaml) cannot be deleted via the portal")
		return
	}
	if err := p.st.DeleteDrive(id); err != nil {
		p.log.Error("drive store delete failed", "id", id, "err", err)
		respondError(w, http.StatusInternalServerError, "store error")
		return
	}
	if err := p.registry.Remove(id); err != nil {
		p.log.Error("registry removal failed after delete", "id", id, "err", err)
	}
	if err := p.st.InsertEvent(id, "released",
		"drive released; OpenList storage "+strconv.FormatInt(d.StorageID, 10)+
			" (mount "+d.MountPath+") kept untouched — delete the storage in the OpenList admin UI if that is intended"); err != nil {
		p.log.Warn("event insert failed", "err", err)
	}
	respondJSON(w, http.StatusOK, map[string]any{"code": http.StatusOK, "message": "success"})
}

func (p *Portal) respondUpstreamError(w http.ResponseWriter, err error) {
	var ue *UpstreamError
	if errors.As(err, &ue) {
		status := ue.Code
		if status < 100 || status > 599 {
			status = http.StatusBadGateway
		}
		respondJSON(w, status, map[string]any{"code": ue.Code, "message": ue.Message})
		return
	}
	p.log.Warn("openlist admin call failed", "err", err)
	respondJSON(w, http.StatusBadGateway, map[string]any{"code": http.StatusBadGateway, "message": "openlist unreachable"})
}
