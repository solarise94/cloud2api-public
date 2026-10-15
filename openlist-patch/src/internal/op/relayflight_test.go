package op

import (
	"context"
	"fmt"
	"path"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/OpenListTeam/OpenList/v4/internal/driver"
	"github.com/OpenListTeam/OpenList/v4/internal/errs"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/pkg/goapiclient"
)

// These tests pin the relay-cycle flight semantics of op's singleflight
// groups (lab finding N6h): a request that arrives as a relay transit —
// context marked with a hop by the ingress middleware — must never join the
// in-flight flight of its own ancestor for the same (storage, path). That
// self-join deadlocks a closed relay cycle at hop 2, before the ingress hop
// cap (MaxHops+1) is ever reached. Requests marked with the same hop must
// still coalesce with each other, and unmarked (non-relay) traffic must keep
// the plain base key and its duplicate-call suppression. Every blocking
// point is real (the tests go through List/Link/MakeDir and an actual
// singleflight join), and every wait is bounded so a regression fails
// instead of hanging the suite.

const (
	hopNone      = -1 // sentinel: context not marked as a relay transit
	entryTimeout = 5 * time.Second
	noMoreWithin = 300 * time.Millisecond
)

// flightDriver is a minimal relay-aware driver: every blocking observation
// point reports the relay hop of the context it ran under and blocks until
// that hop's release channel is closed. All state is write-once before the
// goroutines start or communicated through channels, so the tests are
// race-detector clean.
type flightDriver struct {
	st  model.Storage
	cfg driver.Config

	// getIsFolder selects what Get resolves paths to (directories for
	// List/MakeDir flows, files for the Link flow).
	getIsFolder bool
	// makeDirProbe, when set, is the storage-relative path prefix under
	// which unmarked Get lookups block (the ancestor MakeDir) while marked
	// ones report the object missing, so a descendant MakeDir fails fast
	// without blocking.
	makeDirProbe string

	listEnt, linkEnt, getEnt chan int
	listRel, linkRel, getRel map[int]chan struct{}
}

// flightRunSeq gives every test run its own mount path: op.Link caches
// results for the cache TTL, and go test -count=N reruns the same test in the
// same process, so shared keys would return from cache without ever reaching
// the driver.
var flightRunSeq atomic.Int64

func uniqueMount(base string) string {
	return fmt.Sprintf("%s-%d", base, flightRunSeq.Add(1))
}

func newFlightDriver(mount string) *flightDriver {
	d := &flightDriver{
		st:          model.Storage{MountPath: mount, Status: "work", CacheExpiration: 30},
		cfg:         driver.Config{Name: "flight", NoCache: true},
		getIsFolder: true,
		listEnt:     make(chan int, 8),
		linkEnt:     make(chan int, 8),
		getEnt:      make(chan int, 8),
		listRel:     make(map[int]chan struct{}),
		linkRel:     make(map[int]chan struct{}),
		getRel:      make(map[int]chan struct{}),
	}
	for _, h := range []int{hopNone, 0, 2, 3} {
		d.listRel[h] = make(chan struct{})
		d.linkRel[h] = make(chan struct{})
		d.getRel[h] = make(chan struct{})
	}
	return d
}

func hopOf(ctx context.Context) int {
	if h, ok := goapiclient.HopFromContext(ctx); ok {
		return h
	}
	return hopNone
}

func (d *flightDriver) Config() driver.Config          { return d.cfg }
func (d *flightDriver) GetStorage() *model.Storage     { return &d.st }
func (d *flightDriver) SetStorage(s model.Storage)     { d.st = s }
func (d *flightDriver) GetAddition() driver.Additional { return nil }
func (d *flightDriver) Init(context.Context) error     { return nil }
func (d *flightDriver) Drop(context.Context) error     { return nil }

func (d *flightDriver) List(ctx context.Context, dir model.Obj, args model.ListArgs) ([]model.Obj, error) {
	hop := hopOf(ctx)
	d.listEnt <- hop
	select {
	case <-d.listRel[hop]:
		return []model.Obj{&model.Object{Name: "leaf.txt", Path: path.Join(dir.GetPath(), "leaf.txt")}}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (d *flightDriver) Link(ctx context.Context, file model.Obj, args model.LinkArgs) (*model.Link, error) {
	hop := hopOf(ctx)
	d.linkEnt <- hop
	select {
	case <-d.linkRel[hop]:
		return &model.Link{URL: "http://flight.invalid/" + file.GetName()}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (d *flightDriver) Get(ctx context.Context, p string) (model.Obj, error) {
	if d.makeDirProbe != "" && strings.HasPrefix(p, d.makeDirProbe) {
		if hopOf(ctx) != hopNone {
			// Marked (relay) lookups report the object missing so the
			// descendant MakeDir recurses to the mount root and errors
			// there, never blocking on the ancestor.
			return nil, errs.ObjectNotFound
		}
		d.getEnt <- hopNone
		select {
		case <-d.getRel[hopNone]:
			return &model.Object{Path: p, Name: path.Base(p), IsFolder: true}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return &model.Object{Path: p, Name: path.Base(p), IsFolder: d.getIsFolder}, nil
}

type listResult struct {
	objs []model.Obj
	err  error
}

type linkResult struct {
	link *model.Link
	obj  model.Obj
	err  error
}

func startList(ctx context.Context, d *flightDriver, dir string) <-chan listResult {
	res := make(chan listResult, 1)
	go func() {
		objs, err := List(ctx, d, dir, model.ListArgs{SkipHook: true})
		res <- listResult{objs: objs, err: err}
	}()
	return res
}

func startLink(ctx context.Context, d *flightDriver, p string) <-chan linkResult {
	res := make(chan linkResult, 1)
	go func() {
		link, obj, err := Link(ctx, d, p, model.LinkArgs{Type: "t"})
		res <- linkResult{link: link, obj: obj, err: err}
	}()
	return res
}

func startMakeDir(ctx context.Context, d *flightDriver, p string) <-chan error {
	res := make(chan error, 1)
	go func() {
		res <- MakeDir(ctx, d, p)
	}()
	return res
}

func waitEnter(t *testing.T, ch <-chan int, want int) {
	t.Helper()
	select {
	case got := <-ch:
		if got != want {
			t.Fatalf("driver entry hop %d, want %d", got, want)
		}
	case <-time.After(entryTimeout):
		t.Fatalf("driver entry for hop %d never happened", want)
	}
}

func enterNoMore(t *testing.T, ch <-chan int) {
	t.Helper()
	select {
	case got := <-ch:
		t.Fatalf("unexpected driver entry with hop %d (a flight joined that must not)", got)
	case <-time.After(noMoreWithin):
	}
}

func awaitList(t *testing.T, res <-chan listResult, wantErr bool) listResult {
	t.Helper()
	select {
	case r := <-res:
		if (r.err != nil) != wantErr {
			t.Fatalf("List err=%v, wantErr=%v", r.err, wantErr)
		}
		return r
	case <-time.After(entryTimeout):
		t.Fatalf("List did not return within %s (flight join deadlock?)", entryTimeout)
		return listResult{}
	}
}

func awaitLink(t *testing.T, res <-chan linkResult, wantErr bool) linkResult {
	t.Helper()
	select {
	case r := <-res:
		if (r.err != nil) != wantErr {
			t.Fatalf("Link err=%v, wantErr=%v", r.err, wantErr)
		}
		return r
	case <-time.After(entryTimeout):
		t.Fatalf("Link did not return within %s (flight join deadlock?)", entryTimeout)
		return linkResult{}
	}
}

func awaitMakeDir(t *testing.T, res <-chan error, wantErr bool) {
	t.Helper()
	select {
	case err := <-res:
		if (err != nil) != wantErr {
			t.Fatalf("MakeDir err=%v, wantErr=%v", err, wantErr)
		}
	case <-time.After(entryTimeout):
		t.Fatalf("MakeDir did not return within %s (flight join deadlock?)", entryTimeout)
	}
}

func TestRelayFlightKeyNamespacing(t *testing.T) {
	base := "/mnt/d"
	if got := relayFlightKey(context.Background(), base); got != base {
		t.Fatalf("unmarked context must keep the base key, got %q", got)
	}
	h0 := relayFlightKey(goapiclient.WithHop(context.Background(), 0), base)
	h2 := relayFlightKey(goapiclient.WithHop(context.Background(), 2), base)
	if h0 == base {
		t.Fatal("marked hop 0 must be namespaced away from the base key")
	}
	if h0 == h2 {
		t.Fatal("different relay depths must not share a flight key")
	}
	if !strings.HasPrefix(h0, base) || !strings.HasPrefix(h2, base) {
		t.Fatalf("namespaced keys must keep the base as prefix, got %q and %q", h0, h2)
	}
}

// TestRelayDescendantListDoesNotJoinAncestorFlight is the N6h regression: a
// listing re-entered as a relay transit (hop 2) while the node's own flight
// for the same key is blocked on its outbound hop must run its own flight and
// finish without the ancestor, instead of joining it and deadlocking.
func TestRelayDescendantListDoesNotJoinAncestorFlight(t *testing.T) {
	d := newFlightDriver(uniqueMount("/fl-list"))

	anc := startList(context.Background(), d, "/d")
	waitEnter(t, d.listEnt, hopNone) // ancestor flight in flight, blocked

	desc := startList(goapiclient.WithHop(context.Background(), 2), d, "/d")
	waitEnter(t, d.listEnt, 2) // descendant must run its own List

	// Releasing only the descendant lets it complete while the ancestor is
	// still blocked — impossible if the two shared a flight.
	close(d.listRel[2])
	awaitList(t, desc, false)
	select {
	case <-anc:
		t.Fatal("ancestor flight finished although it was never released")
	default:
	}

	close(d.listRel[hopNone])
	awaitList(t, anc, false)
}

// TestUnmarkedCallerDoesNotJoinRelayListFlight checks the reverse direction:
// a plain unmarked listing must not be serialized behind a relay flight for
// the same key either.
func TestUnmarkedCallerDoesNotJoinRelayListFlight(t *testing.T) {
	d := newFlightDriver(uniqueMount("/fl-rev"))

	relay := startList(goapiclient.WithHop(context.Background(), 2), d, "/d")
	waitEnter(t, d.listEnt, 2)

	plain := startList(context.Background(), d, "/d")
	waitEnter(t, d.listEnt, hopNone)

	close(d.listRel[hopNone])
	awaitList(t, plain, false)
	close(d.listRel[2])
	awaitList(t, relay, false)
}

// TestSameHopRelayListCoalesces pins that duplicate suppression still works
// between requests at the same relay depth.
func TestSameHopRelayListCoalesces(t *testing.T) {
	d := newFlightDriver(uniqueMount("/fl-dedup-hop"))

	first := startList(goapiclient.WithHop(context.Background(), 3), d, "/d")
	waitEnter(t, d.listEnt, 3)

	second := startList(goapiclient.WithHop(context.Background(), 3), d, "/d")
	enterNoMore(t, d.listEnt) // must join the in-flight hop-3 flight

	close(d.listRel[3])
	awaitList(t, first, false)
	awaitList(t, second, false)
}

// TestUnmarkedListCoalescingUnchanged pins that non-relay traffic keeps the
// plain base key and its duplicate-call suppression.
func TestUnmarkedListCoalescingUnchanged(t *testing.T) {
	d := newFlightDriver(uniqueMount("/fl-dedup-plain"))

	first := startList(context.Background(), d, "/d")
	waitEnter(t, d.listEnt, hopNone)

	second := startList(context.Background(), d, "/d")
	enterNoMore(t, d.listEnt) // must join the plain flight

	close(d.listRel[hopNone])
	awaitList(t, first, false)
	awaitList(t, second, false)
}

// TestRelayLinkDoesNotJoinAncestorFlight covers the linkG group: a Link
// re-entered as a relay transit must not join the ancestor link flight.
func TestRelayLinkDoesNotJoinAncestorFlight(t *testing.T) {
	d := newFlightDriver(uniqueMount("/fl-link"))
	d.getIsFolder = false // Link needs a file object

	anc := startLink(context.Background(), d, "/lnk")
	waitEnter(t, d.linkEnt, hopNone)

	desc := startLink(goapiclient.WithHop(context.Background(), 2), d, "/lnk")
	waitEnter(t, d.linkEnt, 2)

	close(d.linkRel[2])
	awaitLink(t, desc, false)
	select {
	case <-anc:
		t.Fatal("ancestor link flight finished although it was never released")
	default:
	}

	close(d.linkRel[hopNone])
	awaitLink(t, anc, false)
}

// TestRelayMakeDirDoesNotJoinAncestorFlight covers the mkdirG group: a
// MakeDir re-entered as a relay transit must not join the ancestor mkdir
// flight. The descendant is expected to fail (its lookups report the object
// missing and it errors out at the mount root) — what matters is that it
// returns promptly instead of blocking on the ancestor.
func TestRelayMakeDirDoesNotJoinAncestorFlight(t *testing.T) {
	d := newFlightDriver(uniqueMount("/fl-mk"))
	d.makeDirProbe = "/mk"

	anc := startMakeDir(context.Background(), d, "/mk/d")
	waitEnter(t, d.getEnt, hopNone)

	desc := startMakeDir(goapiclient.WithHop(context.Background(), 2), d, "/mk/d")
	awaitMakeDir(t, desc, true) // own flight: fails fast at the mount root
	enterNoMore(t, d.getEnt)
	select {
	case <-anc:
		t.Fatal("ancestor MakeDir finished although it was never released")
	default:
	}

	close(d.getRel[hopNone])
	awaitMakeDir(t, anc, false) // unmarked lookup resolves the dir: exists
}
