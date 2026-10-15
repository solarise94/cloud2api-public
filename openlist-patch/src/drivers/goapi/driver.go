// Package goapi mounts a remote GoAPI (cloud2api) /v1 instance as OpenList
// storage. Only that explicit protocol is implemented — no generic REST
// compatibility. Wire rules (see pkg/goapiclient): the apikey goes only to
// the configured endpoint via the `apikey` header; redirects are never
// followed; downloads are proxied through OpenList with a RangeReader (no
// upstream URL ever reaches a browser); uploads succeed only on a typed
// receipt verified against the streamed bytes; unsupported operations fail
// explicitly.
package goapi

import (
	"context"
	"fmt"
	stdpath "path"
	"strings"
	"time"

	"github.com/OpenListTeam/OpenList/v4/internal/driver"
	"github.com/OpenListTeam/OpenList/v4/internal/errs"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/pkg/goapiclient"
	"github.com/pkg/errors"
)

type GoAPI struct {
	model.Storage
	Addition

	// cli/drive are resolved by Init and required afterwards.
	cli   *goapiclient.Client
	drive string
}

func (d *GoAPI) Config() driver.Config {
	return config
}

func (d *GoAPI) GetAddition() driver.Additional {
	return &d.Addition
}

// Init validates the endpoint, the key and the configured root against the
// peer before the storage becomes usable: /v1/me (grants), stat of the root
// (a known directory) and a first listing page (list permission).
func (d *GoAPI) Init(ctx context.Context) error {
	cli, err := goapiclient.New(d.URL, d.APIKey)
	if err != nil {
		return err
	}
	me, err := cli.Me(ctx)
	if err != nil {
		return fmt.Errorf("goapi init: cannot reach the peer /v1/me: %s", mapErrText(err))
	}
	// Without the peer's hop capability the relay loop bound is not
	// enforceable across it: refuse the connection instead of trusting a
	// peer that resets the counter.
	if err := me.ValidateRelayCapability(); err != nil {
		return fmt.Errorf("goapi init: %v", err)
	}
	drive := strings.TrimSpace(d.Addition.Drive)
	if drive == "" {
		drive = me.Key.DefaultDrive
	}
	if drive == "" && len(me.Drives) == 1 {
		drive = me.Drives[0].ID
	}
	if drive == "" || !me.HasDrive(drive) {
		// The peer's default drive counts only inside its explicit grants.
		return fmt.Errorf("goapi init: drive %q is not among the key's granted drives [%s]",
			display(drive), strings.Join(me.GrantedIDs(), ", "))
	}
	if err := goapiclient.ValidDriveID(drive); err != nil {
		return errors.New("goapi init: the configured upstream drive id is invalid")
	}
	root := strings.TrimSpace(d.RootFolderPath)
	if root == "" {
		root = "/"
	}
	rootRemote, err := goapiclient.RemoteJoin(root, "")
	if err != nil {
		return errors.New("goapi init: invalid root path")
	}
	// A root inside the reserved virtual system directories would put the
	// whole storage underneath a view this driver manages itself; refuse the
	// configuration before any probe (see system.go for the view).
	rootClean, err := goapiclient.CleanRel(root)
	if err != nil {
		return errors.New("goapi init: invalid root path")
	}
	for _, reserved := range []string{virtualTrash, virtualRelay} {
		if rootClean == reserved || strings.HasPrefix(rootClean, reserved+"/") {
			return fmt.Errorf("goapi init: the root must not be inside the reserved %s directory", reserved)
		}
	}
	st, err := cli.Stat(ctx, drive, rootRemote)
	if err != nil {
		if isNotFound(err) {
			return fmt.Errorf("goapi init: root %q does not exist on the peer", rootRemote)
		}
		return fmt.Errorf("goapi init: cannot stat the root: %s", mapErrText(err))
	}
	if !st.IsDir {
		return fmt.Errorf("goapi init: root %q is not a directory", rootRemote)
	}
	// The storage is unusable without list permission; probe one page for an
	// early, honest failure (and to check the peer honors the contract).
	if _, err := cli.List(ctx, drive, rootRemote, 1); err != nil {
		return fmt.Errorf("goapi init: cannot list the root: %s", mapErrText(err))
	}
	if err := d.establishSystemView(ctx, cli, drive, root, rootRemote); err != nil {
		return err
	}
	d.cli = cli
	d.drive = drive
	return nil
}

// establishSystemView validates the storage's private system namespace that
// backs the reserved virtual system view (/.c2a-trash, /.relay — system.go)
// and adopts a fresh one when absent. A fresh candidate is committed to the
// addition only after both alias probes passed — OpenList persists the
// addition even when Init fails, so anything earlier would make the next
// init skip the adoption and claim an unverified path. A retained namespace
// is kept as it is, and a missing row identity adopts nothing.
func (d *GoAPI) establishSystemView(ctx context.Context, cli *goapiclient.Client, drive, root, rootRemote string) error {
	rootClean, err := goapiclient.CleanRel(root)
	if err != nil {
		return errors.New("goapi init: invalid root path")
	}
	ns := strings.TrimSpace(d.Addition.SystemNS)
	fresh := false
	switch {
	case ns == "":
		generated, gerr := newNamespace()
		if gerr != nil {
			return gerr
		}
		ns, fresh = generated, true
	case !validNamespace(ns):
		return errors.New("goapi init: system_ns is not a canonical driver namespace; clear the field to have the driver generate a fresh one")
	default:
		d.Addition.SystemNS = ns
	}
	trash, relay, aerr := aliasesFor(ns, d.Storage.ID)
	if aerr != nil {
		// Without the row identity the mapping simply stays fail-closed;
		// nothing here may block the root probes that already passed, and a
		// fresh candidate is discarded so a later init retries the adoption.
		return nil
	}
	for _, alias := range []string{trash, relay} {
		if rootClean == alias || strings.HasPrefix(rootClean, alias+"/") {
			return errors.New("goapi init: the root must not be inside this storage's own system directories")
		}
	}
	if !fresh {
		return nil
	}
	// Adoption probe: a freshly generated namespace has never owned a path,
	// so an existing alias would be an unrelated user path.
	for _, alias := range []string{trash, relay} {
		p := joinRemote(rootRemote, alias)
		_, statErr := cli.Stat(ctx, drive, p)
		switch {
		case statErr == nil:
			return fmt.Errorf("goapi init: %s already exists on the peer; it cannot be claimed as this storage's system directory — remove or rename it, or clear system_ns to adopt a fresh namespace", p)
		case isNotFound(statErr):
			continue
		default:
			return fmt.Errorf("goapi init: cannot verify the system directory on the peer: %s", mapErrText(statErr))
		}
	}
	d.Addition.SystemNS = ns
	return nil
}

func (d *GoAPI) Drop(ctx context.Context) error {
	d.cli = nil
	return nil
}

func (d *GoAPI) client() (*goapiclient.Client, string, error) {
	if d.cli == nil {
		return nil, "", errors.WithStack(errs.StorageNotInit)
	}
	return d.cli, d.drive, nil
}

// remotePath validates that p is the configured root or below it and returns
// the cleaned remote path.
func (d *GoAPI) remotePath(p string) (string, error) {
	return goapiclient.WithinRoot(d.RootFolderPath, p)
}

func (d *GoAPI) List(ctx context.Context, dir model.Obj, args model.ListArgs) ([]model.Obj, error) {
	ctx = goapiclient.Transit(ctx)
	cli, drive, err := d.client()
	if err != nil {
		return nil, err
	}
	remote, err := d.remotePath(dir.GetPath())
	if err != nil {
		return nil, err
	}
	remote, err = d.mapToPeer(remote)
	if err != nil {
		return nil, err
	}
	entries, err := cli.ListAll(ctx, drive, remote)
	if err != nil {
		return nil, mapErr(err)
	}
	// The peer's entry paths are remote-absolute; OpenList expects paths in
	// the same space it handed us (dir.GetPath() is that space), so the peer
	// paths pass through after CheckEntry validated their shape — except
	// this storage's own backing aliases, which return to their reserved
	// virtual names. A rewrite that collides with another entry means the
	// peer state conflicts with the virtual view and fails the listing.
	files := make([]model.Obj, 0, len(entries))
	seen := make(map[string]struct{}, len(entries))
	for _, e := range entries {
		path, name := d.presentEntry(e.Path, e.Name)
		if _, dup := seen[path]; dup {
			return nil, fmt.Errorf("goapi: peer listing conflicts with the reserved system view at %s", path)
		}
		seen[path] = struct{}{}
		files = append(files, &model.ObjThumb{
			Object: model.Object{
				Name:     name,
				Path:     path,
				Modified: parseTime(e.Modified),
				Size:     e.Size,
				IsFolder: e.IsDir,
			},
		})
	}
	return files, nil
}

// presentEntry maps one listed entry back into the virtual view, rewriting
// the name along with the path so both stay consistent.
func (d *GoAPI) presentEntry(path, name string) (string, string) {
	mapped := d.mapFromPeer(path)
	if mapped == path {
		return path, name
	}
	return mapped, stdpath.Base(mapped)
}

// Get implements driver.Getter: op.Get consults it when the directory cache
// holds no entry for the path — exactly the case right after this driver
// created a reserved system root, an upload landed or a trash move ran,
// because the peer listing that fed the cache predates the change and the
// op layer will not re-list a cached directory. The path arrives in the
// OpenList view (not yet joined with the configured root); the returned
// object is presented in the virtual view like a listing entry.
func (d *GoAPI) Get(ctx context.Context, path string) (model.Obj, error) {
	ctx = goapiclient.Transit(ctx)
	cli, drive, err := d.client()
	if err != nil {
		return nil, err
	}
	remote, err := goapiclient.RemoteJoin(d.RootFolderPath, path)
	if err != nil {
		return nil, err
	}
	remote, err = d.mapToPeer(remote)
	if err != nil {
		return nil, err
	}
	st, err := cli.Stat(ctx, drive, remote)
	if err != nil {
		if isNotFound(err) {
			return nil, errors.WithMessage(errs.ObjectNotFound, "goapi: object not found on the peer")
		}
		return nil, mapErr(err)
	}
	// The client enforces the exact path echo; the peer's name must be that
	// path's base, while the presented name comes from the virtual view.
	if st.Name != "" && st.Name != stdpath.Base(st.Path) {
		return nil, fmt.Errorf("%w: stat name does not match the path", goapiclient.ErrBadPeerResponse)
	}
	if st.Size < 0 {
		return nil, fmt.Errorf("%w: negative size", goapiclient.ErrBadPeerResponse)
	}
	virtual := d.mapFromPeer(st.Path)
	return &model.ObjThumb{Object: model.Object{
		Name:     stdpath.Base(virtual),
		Path:     virtual,
		Size:     st.Size,
		IsFolder: st.IsDir,
		Modified: parseTime(st.Modified),
	}}, nil
}

// Link returns a proxied RangeReader. The peer URL and the apikey never
// leave this package: the browser gets OpenList's own proxy endpoint, the
// reader fetches with the key server-side, and every peer redirect is
// refused (no key-bearing redirects are possible). Only the Range derived
// from the consumer's read request is forwarded.
func (d *GoAPI) Link(ctx context.Context, file model.Obj, args model.LinkArgs) (*model.Link, error) {
	ctx = goapiclient.Transit(ctx)
	cli, drive, err := d.client()
	if err != nil {
		return nil, err
	}
	remote, err := d.remotePath(file.GetPath())
	if err != nil {
		return nil, err
	}
	remote, err = d.mapToPeer(remote)
	if err != nil {
		return nil, err
	}
	linkHop, _ := goapiclient.HopFromContext(ctx)
	return &model.Link{
		RangeReader: streamRangeReader(cli, drive, remote, linkHop),
	}, nil
}

func (d *GoAPI) MakeDir(ctx context.Context, parentDir model.Obj, dirName string) error {
	ctx = goapiclient.Transit(ctx)
	cli, drive, err := d.client()
	if err != nil {
		return err
	}
	if err := goapiclient.CheckName(dirName); err != nil {
		return err
	}
	parent, err := d.remotePath(parentDir.GetPath())
	if err != nil {
		return err
	}
	// Map the final joined path, not the parent: creating a reserved system
	// root itself (mkdir "/", ".c2a-trash") must land on the backing alias,
	// and a name joining onto an alias must stay blocked.
	final, err := d.mapToPeer(joinRemote(parent, dirName))
	if err != nil {
		return err
	}
	if err := cli.Mkdir(ctx, drive, final); err != nil {
		return mapErr(err)
	}
	return nil
}

func (d *GoAPI) Move(ctx context.Context, srcObj, dstDir model.Obj) error {
	return d.move(ctx, srcObj, dstDir.GetPath(), srcObj.GetName())
}

func (d *GoAPI) Rename(ctx context.Context, srcObj model.Obj, newName string) error {
	return d.move(ctx, srcObj, stdpath.Dir(srcObj.GetPath()), newName)
}

func (d *GoAPI) move(ctx context.Context, srcObj model.Obj, dstDir, newName string) error {
	ctx = goapiclient.Transit(ctx)
	cli, drive, err := d.client()
	if err != nil {
		return err
	}
	if err := goapiclient.CheckName(newName); err != nil {
		return err
	}
	from, err := d.remotePath(srcObj.GetPath())
	if err != nil {
		return err
	}
	from, err = d.mapToPeer(from)
	if err != nil {
		return err
	}
	dst, err := d.remotePath(dstDir)
	if err != nil {
		return err
	}
	// Same rule as MakeDir: the joined destination is what gets mapped, so
	// a reserved root name resolves through its alias and an alias name
	// cannot be (re)created at the root.
	final, err := d.mapToPeer(joinRemote(dst, newName))
	if err != nil {
		return err
	}
	if err := cli.Move(ctx, drive, from, final); err != nil {
		return mapErr(err)
	}
	return nil
}

func (d *GoAPI) Remove(ctx context.Context, obj model.Obj) error {
	ctx = goapiclient.Transit(ctx)
	cli, drive, err := d.client()
	if err != nil {
		return err
	}
	p, err := d.remotePath(obj.GetPath())
	if err != nil {
		return err
	}
	p, err = d.mapToPeer(p)
	if err != nil {
		return err
	}
	if err := cli.Remove(ctx, drive, p); err != nil {
		return mapErr(err)
	}
	return nil
}

// Put uploads one file. The request is sent exactly once (no retries) and
// the streamed bytes are counted and hashed; success requires the peer's
// typed receipt to match them. A non-conforming receipt, an error or a
// disconnect is a failure — the target is never touched afterwards (no
// compensating delete: after an overwrite it could destroy a user file).
// Unknown and zero sizes go through the peer's multipart form, since its
// raw-body form requires a positive Content-Length; op.Put resolves unknown
// sizes through its cache, so a genuinely unknown size is rare but handled.
func (d *GoAPI) Put(ctx context.Context, dstDir model.Obj, file model.FileStreamer, up driver.UpdateProgress) error {
	ctx = goapiclient.Transit(ctx)
	cli, drive, err := d.client()
	if err != nil {
		return err
	}
	if err := goapiclient.CheckName(file.GetName()); err != nil {
		return err
	}
	dir, err := d.remotePath(dstDir.GetPath())
	if err != nil {
		return err
	}
	// Map the final joined target, not the parent (same rule as MakeDir);
	// the typed receipt must confirm exactly this peer path, so uploads into
	// the reserved virtual view are confirmed against the backing alias,
	// never against the virtual name, and an alias name cannot be (re)
	// created at the root.
	target := joinRemote(dir, file.GetName())
	target, err = d.mapToPeer(target)
	if err != nil {
		return err
	}
	onConflict := "fail"
	if file.GetExist() != nil {
		// The OpenList fs layer decided this upload replaces the existing
		// object (op.Put sets it only on its explicit overwrite path).
		onConflict = "overwrite"
	}
	prog := &driver.ReaderUpdatingProgress{Reader: file, UpdateProgress: up}
	limited := driver.NewLimitedUploadStream(ctx, prog)
	hc := newHashCounter(limited)
	receipt, err := cli.Upload(ctx, drive, target, file.GetName(), onConflict, file.GetSize(), hc)
	if err != nil {
		return mapErr(err)
	}
	if file.GetSize() > 0 && hc.n != file.GetSize() {
		// A partial write must never pass as a confirmed upload.
		return fmt.Errorf("%w: only %d of %d bytes were streamed to the peer", goapiclient.ErrReceipt, hc.n, file.GetSize())
	}
	if err := goapiclient.VerifyReceipt(receipt, target, hc.n, hc.crc.Sum64(), hc.sha.Sum(nil)); err != nil {
		return err
	}
	return nil
}

// ---- small helpers ----

// joinRemote appends a validated name to a validated remote directory.
func joinRemote(dir, name string) string {
	if dir == "" || dir == "/" {
		return "/" + name
	}
	return dir + "/" + name
}

func isNotFound(err error) bool {
	var pe *goapiclient.Error
	if errors.As(err, &pe) {
		return pe.NotFound
	}
	return false
}

// mapErr converts a client error for the OpenList fs layer: only
// path_not_found becomes errs.ObjectNotFound (drives "does not exist");
// everything else is an opaque, already-scrubbed failure.
func mapErr(err error) error {
	if err == nil {
		return nil
	}
	if isNotFound(err) {
		return errors.WithMessage(errs.ObjectNotFound, "goapi: object not found on the peer")
	}
	return errors.Errorf("goapi upstream: %s", mapErrText(err))
}

// mapErrText renders any error as a single-line, bounded display string.
// goapiclient already scrubs the apikey and truncates peer messages.
func mapErrText(err error) string {
	if err == nil {
		return ""
	}
	s := strings.ReplaceAll(err.Error(), "\n", " ")
	if len(s) > 300 {
		s = s[:300] + "…"
	}
	return s
}

// display quotes a value for an error message without revealing more than
// the administrator already typed into the form.
func display(s string) string {
	if s == "" {
		return ""
	}
	return s
}

func parseTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02 15:04:05 -0700 MST", "2006-01-02 15:04:05 MST"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return time.Time{}
}
