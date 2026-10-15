package agentapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash/crc64"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"cloud2api/internal/auth"
	"cloud2api/internal/keymgr"
	"cloud2api/internal/store"
)

// ---- POST /v1/selftest (§4.10.1) ----

// selftestDir is the per-key scratch directory inside every writable root.
const selftestDir = "/.c2a-selftest"

func (s *Server) handleSelftest(c *call) {
	type driveResult struct {
		Drive      string `json:"drive"`
		OK         bool   `json:"ok"`
		LatencyMS  int64  `json:"latency_ms"`
		Error      string `json:"error,omitempty"`
		WroteBytes int    `json:"wrote_bytes"`
		CRC64      string `json:"crc64,omitempty"`
	}
	results := make([]driveResult, 0, len(c.key.Drives))
	allOK := true
	for _, g := range c.key.Drives {
		res := driveResult{Drive: g.Drive}
		start := time.Now()
		if !c.key.CanOn(g.Drive, auth.PermWrite) {
			res.Error = "skipped: no write permission on this drive"
			results = append(results, res)
			continue
		}
		n, crc, err := s.selftestOne(c.r.Context(), c.key, g.Drive, g.Root)
		if err != nil {
			res.Error = err.Error()
			allOK = false
		} else {
			res.OK = true
			res.WroteBytes = n
			res.CRC64 = crc
		}
		res.LatencyMS = time.Since(start).Milliseconds()
		results = append(results, res)
	}
	_ = s.st.TouchKeySelftest(c.key.ID)
	c.ok(map[string]any{
		"drives": results,
		"all_ok": allOK,
		"hint":   "each drive got a small file written, read back, CRC64-verified and permanently removed",
	})
}

// selftestOne writes a small file under the key root, reads it back, compares
// CRC64 and deletes it permanently (bypassing the trash so the test leaves no
// residue). It returns the payload size and its CRC64 decimal string.
func (s *Server) selftestOne(ctx context.Context, key *auth.Key, drive, root string) (int, string, error) {
	acct, known := s.registry.Get(drive)
	if !known {
		return 0, "", fmt.Errorf("drive is not routable")
	}
	if !acct.Enabled {
		return 0, "", fmt.Errorf("drive is disabled")
	}
	if verdict := s.guard.Check(ctx, drive); !verdict.Allowed() {
		return 0, "", fmt.Errorf("drive is unavailable: %s", verdict.Message)
	}
	buf := make([]byte, 8)
	_, _ = rand.Read(buf)
	name := "selftest-" + hex.EncodeToString(buf) + ".bin"

	// visible path inside the key root; internal adds mount + root.
	visible := path.Join(selftestDir, name)
	full := joinRoot(root, visible)
	internal := s.toInternal(drive, full)

	cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	dir, _ := parentOf(internal)
	if err := s.mkdirAll(cctx, dir); err != nil {
		return 0, "", fmt.Errorf("mkdir failed: %s", upstreamMessage(err))
	}
	payload := []byte("c2a-selftest " + time.Now().UTC().Format(time.RFC3339Nano) + " " + name)
	crc := crc64.New(crc64Table)
	crc.Write(payload)
	want := crc.Sum64()

	if err := s.putBytes(cctx, internal, payload); err != nil {
		return 0, "", fmt.Errorf("write failed: %s", upstreamMessage(err))
	}
	got, err := s.getBytes(cctx, internal)
	if err != nil {
		_ = s.fsRemove(cctx, dir, []string{name})
		return 0, "", fmt.Errorf("read-back failed: %s", upstreamMessage(err))
	}
	gotCRC := crc64.New(crc64Table)
	gotCRC.Write(got)
	if gotCRC.Sum64() != want {
		_ = s.fsRemove(cctx, dir, []string{name})
		return 0, "", fmt.Errorf("CRC64 mismatch: wrote %d, read back %d", want, gotCRC.Sum64())
	}
	// Delete permanently (bypass the trash).
	if err := s.fsRemove(cctx, dir, []string{name}); err != nil {
		return len(payload), u64decimal(want), fmt.Errorf("cleanup failed: %s", upstreamMessage(err))
	}
	return len(payload), u64decimal(want), nil
}

// putBytes uploads a small buffer through OpenList's fs/put.
func (s *Server) putBytes(ctx context.Context, internal string, data []byte) error {
	hdr := http.Header{}
	hdr.Set("File-Path", escapeUploadPath(internal))
	hdr.Set("Content-Type", "application/octet-stream")
	u, err := url.Parse("/api/fs/put")
	if err != nil {
		return err
	}
	req, err := s.client.NewRequest(ctx, http.MethodPut, *u, strings.NewReader(string(data)), int64(len(data)), hdr)
	if err != nil {
		return err
	}
	resp, err := s.client.DoAutoHeal(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var envelope struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}
	_ = json.Unmarshal(body, &envelope)
	if resp.StatusCode != http.StatusOK || (envelope.Code != 0 && envelope.Code != http.StatusOK) {
		msg := envelope.Message
		if msg == "" {
			msg = resp.Status
		}
		return &upstreamError{Code: envelope.Code, Message: msg}
	}
	return nil
}

// getBytes downloads a small object fully.
func (s *Server) getBytes(ctx context.Context, internal string) ([]byte, error) {
	info, err := s.fsGet(ctx, internal)
	if err != nil {
		return nil, err
	}
	sign := info.Sign
	if sign == "" {
		sign, err = s.fetchSign(ctx, internal)
		if err != nil {
			return nil, err
		}
	}
	u, err := url.Parse("/p" + internal)
	if err != nil {
		return nil, err
	}
	u.RawQuery = "sign=" + sign
	req, err := s.client.NewRequest(ctx, http.MethodGet, *u, nil, 0, nil)
	if err != nil {
		return nil, err
	}
	resp, err := s.client.DoAutoHeal(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, &upstreamError{Code: resp.StatusCode, Message: "download " + resp.Status}
	}
	return io.ReadAll(io.LimitReader(resp.Body, 8<<20))
}

// ---- child keys (§4.10.8) ----

const (
	childKeyDefaultTTL = 24 * time.Hour
	childKeyMaxTTL     = 7 * 24 * time.Hour
)

// subsetError validates that a child key grant is a strict subset of the
// parent's (each drive granted to the parent, child root inside the parent
// root, perms ⊆ parent perms, global ⊆ parent global).
func subsetError(parent *auth.Key, childDrives []store.KeyDrive, childGlobal []string) string {
	seen := map[string]bool{}
	for _, g := range childDrives {
		if seen[g.Drive] {
			return "drive \"" + g.Drive + "\" granted twice"
		}
		seen[g.Drive] = true
		pg, ok := parent.DriveGrant(g.Drive)
		if !ok {
			return "drive \"" + g.Drive + "\" is not granted to the parent key"
		}
		root := g.Root
		if root == "" {
			root = "/"
		}
		proot := path.Clean("/" + pg.Root)
		croot := path.Clean("/" + root)
		if proot != "/" && croot != proot && !strings.HasPrefix(croot, proot+"/") {
			return "child root " + croot + " on drive \"" + g.Drive + "\" escapes the parent root " + proot
		}
		for _, p := range g.Perms {
			if !parent.CanOn(g.Drive, auth.Permission(p)) {
				return "permission \"" + p + "\" on drive \"" + g.Drive + "\" exceeds the parent key"
			}
		}
	}
	for _, p := range childGlobal {
		if !parent.GlobalPerms[auth.Permission(p)] {
			return "global permission \"" + p + "\" exceeds the parent key"
		}
	}
	return ""
}

func (s *Server) handleChildKeyCreate(c *call) {
	if !c.key.GlobalPerms[auth.PermDelegate] {
		c.fail(errForbidden("this key lacks the \"delegate\" global permission and cannot create child keys"))
		return
	}
	var body struct {
		ID          string               `json:"id"`
		Drives      []store.KeyDrive     `json:"drives"`
		GlobalPerms []string             `json:"global_perms"`
		TTLSeconds  int64                `json:"ttl_seconds"`
		Limits      *store.Limits        `json:"limits"`
		Trash       *store.TrashSettings `json:"trash"`
	}
	if !decodeBody(c, &body) {
		return
	}
	if len(body.Drives) == 0 {
		c.fail(errInvalidRequest("field `drives` (array of {drive,root,perms}) is required"))
		return
	}
	if msg := subsetError(c.key, body.Drives, body.GlobalPerms); msg != "" {
		e := errForbidden(msg)
		e.Hint = "child keys must stay within the parent's drives, roots and permissions"
		c.fail(e)
		return
	}
	ttl := childKeyDefaultTTL
	if body.TTLSeconds > 0 {
		ttl = time.Duration(body.TTLSeconds) * time.Second
	}
	if ttl > childKeyMaxTTL {
		c.fail(errInvalidRequest("ttl_seconds must not exceed 604800 (7 days)"))
		return
	}

	spec := store.APIKey{
		ParentID:    c.key.ID,
		ExpiresAt:   timePtr(time.Now().UTC().Add(ttl)),
		Drives:      body.Drives,
		GlobalPerms: body.GlobalPerms,
		Trash:       store.TrashSettings{Enabled: true, Days: 7},
	}
	if body.Trash != nil {
		spec.Trash = *body.Trash
	}
	if body.Limits != nil {
		spec.Limits = *body.Limits
	}
	if strings.TrimSpace(body.ID) != "" {
		spec.ID = strings.TrimSpace(body.ID)
	} else {
		idbuf := make([]byte, 3)
		_, _ = rand.Read(idbuf)
		spec.ID = "ck-" + hex.EncodeToString(idbuf)
	}
	if err := keymgr.ValidateKeySpec(spec, s.knownDrives()); err != nil {
		c.fail(errInvalidRequest(err.Error()))
		return
	}
	created, plaintext, err := s.keys.Create(spec)
	if err != nil {
		if strings.Contains(err.Error(), "already exists") {
			c.fail(errAlreadyExists("child key id \"" + spec.ID + "\""))
			return
		}
		c.fail(errUpstream("child key create failed: " + err.Error()))
		return
	}
	c.ok(map[string]any{
		"key":        childKeyView(created),
		"plaintext":  plaintext,
		"expires_at": created.ExpiresAt.UTC().Format(time.RFC3339),
	})
}

func childKeyView(k store.APIKey) map[string]any {
	drives := make([]map[string]any, 0, len(k.Drives))
	for _, g := range k.Drives {
		root := g.Root
		if root == "" {
			root = "/"
		}
		perms := g.Perms
		if perms == nil {
			perms = []string{}
		}
		drives = append(drives, map[string]any{"drive": g.Drive, "root": root, "perms": perms})
	}
	global := k.GlobalPerms
	if global == nil {
		global = []string{}
	}
	return map[string]any{
		"id":           k.ID,
		"parent_id":    k.ParentID,
		"prefix":       "sk-c2a-" + k.PublicID,
		"created_at":   k.CreatedAt.UTC().Format(time.RFC3339),
		"expires_at":   k.ExpiresAt.UTC().Format(time.RFC3339),
		"revoked":      k.Revoked,
		"drives":       drives,
		"global_perms": global,
	}
}

func (s *Server) knownDrives() map[string]bool {
	out := map[string]bool{}
	drives, err := s.st.ListDrives()
	if err != nil {
		return out
	}
	for _, d := range drives {
		out[d.ID] = true
	}
	return out
}

func (s *Server) handleChildKeysList(c *call) {
	if !c.key.GlobalPerms[auth.PermDelegate] {
		c.fail(errForbidden("this key lacks the \"delegate\" global permission"))
		return
	}
	keys, err := s.keys.List()
	if err != nil {
		c.fail(errUpstream("key listing failed"))
		return
	}
	out := []map[string]any{}
	for _, k := range keys {
		if k.ParentID == c.key.ID {
			out = append(out, childKeyView(k))
		}
	}
	c.ok(map[string]any{"keys": out})
}

// handleChildKeyDelete revokes one of the key's own descendants (§4.10.8).
func (s *Server) handleChildKeyDelete(c *call, id string) {
	if !c.key.GlobalPerms[auth.PermDelegate] {
		c.fail(errForbidden("this key lacks the \"delegate\" global permission"))
		return
	}
	keys, err := s.keys.List()
	if err != nil {
		c.fail(errUpstream("key listing failed"))
		return
	}
	childrenOf := map[string][]string{}
	byID := map[string]store.APIKey{}
	for _, k := range keys {
		byID[k.ID] = k
		if k.ParentID != "" {
			childrenOf[k.ParentID] = append(childrenOf[k.ParentID], k.ID)
		}
	}
	// Walk descendants of c.key.ID.
	descendants := map[string]bool{}
	stack := append([]string{}, childrenOf[c.key.ID]...)
	for len(stack) > 0 {
		cur := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if descendants[cur] {
			continue
		}
		descendants[cur] = true
		stack = append(stack, childrenOf[cur]...)
	}
	if !descendants[id] {
		e := errForbidden("key \"" + id + "\" is not one of this key's descendants")
		e.Hint = "list child keys with GET /v1/keys"
		c.fail(e)
		return
	}
	if err := s.keys.Revoke(id); err != nil {
		c.fail(errUpstream("revoke failed: " + err.Error()))
		return
	}
	c.ok(map[string]any{"id": id, "revoked": true})
}

// ---- GET /v1/activity (§4.10.9) ----

func (s *Server) handleActivity(c *call) {
	limit, e := parseInt(c.r.URL.Query().Get, "limit", 50, 200)
	if e != nil {
		c.fail(e)
		return
	}
	entries, _, err := s.st.QueryAudit(store.AuditFilter{KeyExact: c.key.ID, Limit: limit})
	if err != nil {
		c.fail(errUpstream("activity query failed"))
		return
	}
	out := make([]map[string]any, 0, len(entries))
	for _, en := range entries {
		out = append(out, map[string]any{
			"ts":          en.TS.UTC().Format(time.RFC3339),
			"drive":       en.Drive,
			"op":          en.Op,
			"path":        en.Path,
			"bytes":       en.Bytes,
			"status":      en.Status,
			"duration_ms": en.DurationMS,
			"error":       en.Error,
		})
	}
	c.ok(map[string]any{"entries": out})
}

func timePtr(t time.Time) *time.Time { return &t }
