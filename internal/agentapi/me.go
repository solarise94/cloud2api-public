package agentapi

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"cloud2api/internal/auth"
	"cloud2api/internal/openlist"
	"cloud2api/internal/storageid"
)

// healthCache snapshots the OpenList storage statuses for at most 30s for
// the display surfaces (/v1/me, /v1/drives — UPGRADE §3.7.1 allows a short
// cache there). The data plane never reads it: it uses the fresh
// storageid.Guard instead, precisely so a stale snapshot cannot authorize
// access to a replaced storage.
type healthCache struct {
	client *openlist.Client

	mu      sync.RWMutex
	at      time.Time
	byMount map[string]openlist.StorageInfo
	byID    map[int64]openlist.StorageInfo
}

func newHealthCache(client *openlist.Client) *healthCache {
	return &healthCache{client: client}
}

// invalidate drops the cached snapshot so the next snapshot() refetches.
func (h *healthCache) invalidate() {
	h.mu.Lock()
	h.at = time.Time{}
	h.mu.Unlock()
}

// snapshot returns both indexes (mount path and storage id).
func (h *healthCache) snapshot(ctx context.Context) (map[string]openlist.StorageInfo, map[int64]openlist.StorageInfo) {
	h.mu.RLock()
	fresh := time.Since(h.at) < 30*time.Second
	byMount, byID := h.byMount, h.byID
	h.mu.RUnlock()
	if fresh {
		return byMount, byID
	}
	list, err := h.client.ListStorages(ctx)
	h.mu.Lock()
	defer h.mu.Unlock()
	h.at = time.Now()
	h.byMount = make(map[string]openlist.StorageInfo, len(list))
	h.byID = make(map[int64]openlist.StorageInfo, len(list))
	for _, st := range list {
		h.byMount[st.MountPath] = st
		if st.ID != 0 {
			h.byID[st.ID] = st
		}
	}
	if err != nil {
		// Keep whatever snapshot exists; the last refresh time still moved
		// so a down OpenList is not hammered on every call.
	}
	return h.byMount, h.byID
}

// driveView is the agent-facing description of one granted drive.
type driveView struct {
	ID       string   `json:"id"`
	Provider string   `json:"provider"`
	Root     string   `json:"root"`
	Perms    []string `json:"perms"`
	Health   string   `json:"health"`
	Default  bool     `json:"default,omitempty"`
}

// driveViews renders one granted drive (display only; the health comes from
// storageid.Resolve over the 30s snapshot, which is allowed to be slightly
// stale — the data plane uses the fresh guard instead).
func (s *Server) driveViews(c *call) []driveView {
	ctx, cancel := context.WithTimeout(c.r.Context(), 15*time.Second)
	defer cancel()
	byMount, byID := s.healthCache.snapshot(ctx)
	def := c.key.EffectiveDefaultDrive()
	drives := s.storeDrives()

	views := make([]driveView, 0, len(c.key.Drives))
	for _, g := range c.key.Drives {
		provider := ""
		ident := storageid.Drive{Enabled: true}
		if sd, ok := drives[g.Drive]; ok {
			provider = sd.Provider
			ident = sd.ID
		}
		if acct, ok := s.registry.Get(g.Drive); ok {
			ident.Enabled = ident.Enabled && acct.Enabled
			ident.MountPath = acct.MountPath
		}
		result := storageid.Resolve(ident, byMount, byID)
		state := result.State
		perms := make([]string, 0, len(g.Perms))
		for _, p := range auth.AllPermissions {
			if g.Perms[p] {
				perms = append(perms, string(p))
			}
		}
		views = append(views, driveView{
			ID:       g.Drive,
			Provider: provider,
			Root:     g.Root,
			Perms:    perms,
			Health:   state,
			Default:  g.Drive == def,
		})
	}
	return views
}

// storeDrives returns id -> store drive metadata (read through per call; the
// drive table is small).
func (s *Server) storeDrives() map[string]struct {
	Provider string
	ID       storageid.Drive
} {
	out := map[string]struct {
		Provider string
		ID       storageid.Drive
	}{}
	drives, err := s.st.ListDrives()
	if err != nil {
		return out
	}
	for _, d := range drives {
		out[d.ID] = struct {
			Provider string
			ID       storageid.Drive
		}{d.Provider, storageid.Drive{
			StorageID: d.StorageID, MountPath: d.MountPath,
			Static: d.Static, Enabled: d.Enabled, State: d.State,
		}}
	}
	return out
}

// ---- handlers ----

// handleIndex renders the /v1 entry index (§4.10.1): links for the basic
// surface (isBasicRoute, index itself excluded), the quickstart and the
// asset URLs.
func (s *Server) handleIndex(c *call) {
	base := c.base
	endpoints := []map[string]string{}
	for _, r := range routeTable {
		if !isBasicRoute(r.Method, r.Path) {
			continue
		}
		endpoints = append(endpoints, map[string]string{
			"method":  r.Method,
			"path":    r.Path,
			"summary": r.Summary,
		})
	}
	c.ok(map[string]any{
		"name":    "cloud2api agent gateway",
		"version": "v0.4-1b",
		"quickstart": []string{
			"GET " + base + "/v1/me — inspect this key (drives, roots, permissions, health)",
			"GET " + base + "/v1/drives/{drive}/list?path=/ — browse a directory (drive ids come from /v1/me)",
			"GET " + base + "/v1/drives/{drive}/files?path=/f — download a file; PUT the same URL to upload",
			"POST " + base + "/v1/transfers — copy/move across drives as a job; poll GET /v1/jobs/{id}?wait=60",
			"GET " + base + "/v1 — this index; the basic endpoints are listed under `endpoints`",
		},
		"endpoints":        endpoints,
		"instructions_url": base + "/custom/agent-instructions.txt",
		"openapi_url":      base + "/apidocumentation/openapi.json",
		"helper_url":       base + "/custom/c2a.py",
		"skill_url":        base + "/custom/SKILL.md",
		"long_poll":        "GET /v1/jobs/{id}?wait=60 returns early on state change",
		"dry_run_rule":     "operations affecting more than 100 files or 1 GiB must be dry-run first (delete, move and transfers accept dry_run=true)",
	})
}

// nextSteps computes the dynamic next_steps array of /v1/me (§4.10.1).
func (s *Server) nextSteps(c *call) []string {
	var steps []string
	views := s.driveViews(c)
	for _, d := range views {
		switch d.Health {
		case "needs_reauth":
			steps = append(steps, fmt.Sprintf(
				"drive %s rejected its stored credentials; an administrator must update them in OpenList (the cloud2api portal links to it)", d.ID))
		case "replacement_found":
			steps = append(steps, fmt.Sprintf(
				"drive %s had its OpenList storage replaced; access is blocked until an administrator confirms the replacement in the cloud2api portal", d.ID))
		case "error", "orphaned", "disabled":
			steps = append(steps, fmt.Sprintf("drive %s is %s; other drives keep working", d.ID, d.Health))
		}
	}
	// Active jobs: poll the known running job ids.
	if jobs, err := s.st.ListJobs(c.key.ID, "running", 5); err == nil && len(jobs) > 0 {
		steps = append(steps, fmt.Sprintf("%d job(s) currently running: wait with GET /v1/jobs/{id}?wait=60", len(jobs)))
	}
	// Trash expiry stays informational: restore is an advanced route and is
	// not advertised here.
	if items, err := s.st.ListTrashForKey(c.key.ID); err == nil {
		soon := []string{}
		for _, it := range items {
			if time.Until(it.PurgeAfter) < 24*time.Hour {
				soon = append(soon, fmt.Sprintf("#%d (%s)", it.ID, it.OrigPath))
			}
		}
		if len(soon) > 0 {
			steps = append(steps, fmt.Sprintf("trash items expiring within 24h: %s — they purge automatically afterwards", strings.Join(soon, ", ")))
		}
	}
	steps = append(steps, "after every task: prefer dry_run=true before deletes/moves affecting >100 files or >1 GiB")
	return steps
}

func (s *Server) handleMe(c *call) {
	globalPerms := make([]string, 0, len(c.key.GlobalPerms))
	for _, p := range auth.AllPermissions {
		if c.key.GlobalPerms[p] {
			globalPerms = append(globalPerms, string(p))
		}
	}
	limits := map[string]any{
		"max_upload_bytes":    nil,
		"requests_per_minute": nil,
		"concurrent_jobs":     nil,
	}
	if c.key.Limits.MaxUploadBytes > 0 {
		limits["max_upload_bytes"] = c.key.Limits.MaxUploadBytes
	}
	if c.key.Limits.RequestsPerMinute > 0 {
		limits["requests_per_minute"] = c.key.Limits.RequestsPerMinute
	}
	if c.key.Limits.ConcurrentJobs > 0 {
		limits["concurrent_jobs"] = c.key.Limits.ConcurrentJobs
	}
	days := c.key.TrashDays
	if days <= 0 {
		days = 7
	}
	var lastSelftest any
	if row, err := s.keys.Get(c.key.ID); err == nil && row.LastSelftest != nil && !row.LastSelftest.IsZero() {
		lastSelftest = row.LastSelftest.UTC().Format(time.RFC3339)
	}
	// Relay (Gokapi compatibility, §4.7): settings plus live usage; enabled
	// reflects the effective `relay` permission on the effective relay drive.
	// The Gokapi onboarding URL is not advertised on the basic surface.
	relayView := map[string]any{
		"enabled":      false,
		"drive":        c.key.EffectiveRelayDrive(),
		"quota_bytes":  c.key.EffectiveRelayQuota(),
		"max_age_days": c.key.EffectiveRelayMaxAge(),
		"used_bytes":   0,
		"files":        0,
	}
	if drive := c.key.EffectiveRelayDrive(); drive != "" && c.key.CanOn(drive, auth.PermRelay) {
		relayView["enabled"] = true
	}
	if files, used, err := s.st.RelayFileCounts(c.key.ID); err == nil {
		relayView["files"] = files
		relayView["used_bytes"] = used
	}
	c.ok(map[string]any{
		"key": map[string]any{
			"id":               c.key.ID,
			"prefix":           c.key.Prefix(),
			"default_drive":    c.key.EffectiveDefaultDrive(),
			"last_selftest_at": lastSelftest,
		},
		"drives":       s.driveViews(c),
		"global_perms": globalPerms,
		"limits":       limits,
		"trash":        map[string]any{"enabled": c.key.TrashOn, "days": days},
		"relay":        relayView,
		// Relay capability (GoAPI chain loop guard): the largest
		// X-GoAPI-Hop value this instance accepts. GoAPI peers require it.
		"relay_hop_limit":  relayHopLimitJSON(),
		"policy_text":      s.policyText(),
		"instructions_url": c.base + "/custom/agent-instructions.txt",
		"openapi_url":      c.base + "/apidocumentation/openapi.json",
		"next_steps":       s.nextSteps(c),
	})
}

func (s *Server) handleDrives(c *call) {
	c.ok(map[string]any{"drives": s.driveViews(c)})
}
