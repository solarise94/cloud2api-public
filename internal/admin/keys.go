package admin

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"time"

	"cloud2api/internal/keymgr"
	"cloud2api/internal/store"
)

var keyIDRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$`)

// keyRow renders one §8.2 keys row (same shape the SPA mock expects).
func keyRow(k store.APIKey) map[string]any {
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
	var parentID any
	if k.ParentID != "" {
		parentID = k.ParentID
	}
	var prefix any
	if k.PublicID != "" {
		prefix = "sk-c2a-" + k.PublicID
	}
	var expiresAt, lastUsed any
	if k.ExpiresAt != nil {
		expiresAt = k.ExpiresAt.UTC().Format(time.RFC3339)
	}
	if k.LastUsedAt != nil && !k.LastUsedAt.IsZero() {
		lastUsed = k.LastUsedAt.UTC().Format(time.RFC3339)
	}
	var defaultDrive any
	if k.DefaultDrive != "" {
		defaultDrive = k.DefaultDrive
	}
	var relayDrive any
	if k.Relay.Drive != "" {
		relayDrive = k.Relay.Drive
	}
	return map[string]any{
		"id":            k.ID,
		"parent_id":     parentID,
		"prefix":        prefix,
		"static":        k.Static,
		"revoked":       k.Revoked,
		"created_at":    k.CreatedAt.UTC().Format(time.RFC3339),
		"expires_at":    expiresAt,
		"last_used_at":  lastUsed,
		"default_drive": defaultDrive,
		"drives":        drives,
		"global_perms":  global,
		"limits":        limitsRow(k.Limits),
		"trash":         map[string]any{"enabled": k.Trash.Enabled, "days": k.Trash.Days},
		"relay": map[string]any{
			"drive":        relayDrive,
			"quota_bytes":  k.Relay.QuotaBytes,
			"max_age_days": k.Relay.MaxAgeDays,
		},
	}
}

func limitsRow(l store.Limits) map[string]any {
	row := map[string]any{
		"max_upload_bytes":    nil,
		"requests_per_minute": nil,
		"concurrent_jobs":     nil,
	}
	if l.MaxUploadBytes != nil {
		row["max_upload_bytes"] = *l.MaxUploadBytes
	}
	if l.RequestsPerMinute != nil {
		row["requests_per_minute"] = *l.RequestsPerMinute
	}
	if l.ConcurrentJobs != nil {
		row["concurrent_jobs"] = *l.ConcurrentJobs
	}
	return row
}

// keyWritable is the POST/PATCH body (§8.2 keys). Presence tracking for
// PATCH comes from the raw shadow map (decodeBodyWithPresence).
type keyWritable struct {
	ID           string               `json:"id"`
	DefaultDrive string               `json:"default_drive"`
	ExpiresAt    *time.Time           `json:"expires_at"`
	Drives       []store.KeyDrive     `json:"drives"`
	GlobalPerms  []string             `json:"global_perms"`
	Limits       *store.Limits        `json:"limits"`
	Trash        *store.TrashSettings `json:"trash"`
	Relay        *store.RelaySettings `json:"relay"`

	present map[string]bool
}

func (b *keyWritable) has(field string) bool {
	return b.present != nil && b.present[field]
}

// decodeKeyBody fills the writable fields and records which top-level keys
// the request actually carried.
func decodeKeyBody(w http.ResponseWriter, r *http.Request, out *keyWritable) bool {
	raw := map[string]json.RawMessage{}
	if err := decodeBody(w, r, &raw); err != nil {
		respondError(w, http.StatusBadRequest, "invalid JSON body")
		return false
	}
	b, _ := json.Marshal(raw)
	if err := json.Unmarshal(b, out); err != nil {
		respondError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return false
	}
	out.present = map[string]bool{}
	for k := range raw {
		out.present[k] = true
	}
	return true
}

func (p *Portal) knownDrives() map[string]bool {
	out := map[string]bool{}
	drives, err := p.st.ListDrives()
	if err != nil {
		return out
	}
	for _, d := range drives {
		out[d.ID] = true
	}
	return out
}

func (p *Portal) handleListKeys(w http.ResponseWriter, r *http.Request) {
	keys, err := p.keys.List()
	if err != nil {
		respondError(w, http.StatusInternalServerError, "store error")
		return
	}
	rows := make([]map[string]any, 0, len(keys))
	for _, k := range keys {
		rows = append(rows, keyRow(k))
	}
	respondData(w, map[string]any{"keys": rows})
}

func (p *Portal) handleCreateKey(w http.ResponseWriter, r *http.Request) {
	var body keyWritable
	if !decodeKeyBody(w, r, &body) {
		return
	}
	id := strings.TrimSpace(body.ID)
	if id == "" {
		id = "key-" + randomHex(4)
	}
	if !keyIDRe.MatchString(id) {
		respondError(w, http.StatusBadRequest, "key id may only contain letters, digits, '-' and '_'")
		return
	}
	spec := keySpecFromBody(body)
	spec.ID = id
	if err := keymgr.ValidateKeySpec(spec, p.knownDrives()); err != nil {
		respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	// Cross-drive keys need an explicit default drive: the /v1 default-drive
	// aliases must not silently depend on grant order.
	if len(spec.Drives) > 1 && spec.DefaultDrive == "" {
		respondError(w, http.StatusBadRequest, "default_drive is required when granting more than one drive")
		return
	}
	created, plaintext, err := p.keys.Create(spec)
	if err != nil {
		respondKeyError(w, err)
		return
	}
	prompt := p.onboardingPrompt(created, plaintext, r)
	respondData(w, map[string]any{
		"key":               keyRow(created),
		"plaintext":         plaintext,
		"onboarding_prompt": prompt,
	})
}

// serveKeyItem routes PATCH /keys/{id}, POST /keys/{id}/rotate,
// DELETE /keys/{id}.
func (p *Portal) serveKeyItem(w http.ResponseWriter, r *http.Request, rest string) {
	id, action := rest, ""
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		id, action = rest[:i], rest[i+1:]
	}
	if id == "" || strings.Contains(id, "/") {
		respondError(w, http.StatusNotFound, "not found")
		return
	}
	switch {
	case action == "" && r.Method == http.MethodPatch:
		p.handlePatchKey(w, r, id)
	case action == "rotate" && r.Method == http.MethodPost:
		p.handleRotateKey(w, r, id)
	case action == "" && r.Method == http.MethodDelete:
		p.handleRevokeKey(w, r, id)
	default:
		respondError(w, http.StatusNotFound, "not found")
	}
}

func (p *Portal) handlePatchKey(w http.ResponseWriter, r *http.Request, id string) {
	var body keyWritable
	if !decodeKeyBody(w, r, &body) {
		return
	}
	updated, err := p.keys.Patch(id, func(k *store.APIKey) error {
		driveChange := body.has("drives") || body.has("default_drive")
		if body.has("default_drive") {
			k.DefaultDrive = body.DefaultDrive
		}
		if body.has("expires_at") {
			k.ExpiresAt = body.ExpiresAt // null clears the expiry
		}
		if body.has("drives") {
			k.Drives = body.Drives
		}
		if body.has("global_perms") {
			k.GlobalPerms = body.GlobalPerms
		}
		if body.has("limits") {
			k.Limits = *body.Limits
		}
		if body.has("trash") {
			k.Trash = *body.Trash
		}
		if body.has("relay") {
			if body.Relay != nil {
				k.Relay = *body.Relay
			} else {
				k.Relay = store.RelaySettings{} // null resets to defaults
			}
		}
		// When the drive set or the default drive changes, a resulting
		// cross-drive key must still name its default explicitly (stored keys
		// with a single grant keep working without one).
		if driveChange && len(k.Drives) > 1 && k.DefaultDrive == "" {
			return errors.New("default_drive is required when the key grants more than one drive")
		}
		return nil
	})
	if err != nil {
		respondKeyError(w, err)
		return
	}
	if err := keymgr.ValidateKeySpec(store.APIKey{
		Drives: updated.Drives, GlobalPerms: updated.GlobalPerms,
		DefaultDrive: updated.DefaultDrive, Limits: updated.Limits, Trash: updated.Trash,
		Relay: updated.Relay,
	}, p.knownDrives()); err != nil {
		respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	respondData(w, keyRow(updated))
}

func (p *Portal) handleRotateKey(w http.ResponseWriter, r *http.Request, id string) {
	updated, plaintext, err := p.keys.Rotate(id)
	if err != nil {
		respondKeyError(w, err)
		return
	}
	respondData(w, map[string]any{
		"key":               keyRow(updated),
		"plaintext":         plaintext,
		"onboarding_prompt": p.onboardingPrompt(updated, plaintext, r),
	})
}

func (p *Portal) handleRevokeKey(w http.ResponseWriter, r *http.Request, id string) {
	if err := p.keys.Revoke(id); err != nil {
		respondKeyError(w, err)
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{"code": http.StatusOK, "message": "success"})
}

func respondKeyError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, keymgr.ErrStaticKey):
		respondError(w, http.StatusConflict, "static keys (config.yaml) are read-only")
	case errors.Is(err, store.ErrNotFound):
		respondError(w, http.StatusNotFound, "key not found")
	default:
		respondError(w, http.StatusBadRequest, err.Error())
	}
}

// keySpecFromBody converts the admin body into a store key spec.
func keySpecFromBody(b keyWritable) store.APIKey {
	trash := store.TrashSettings{Enabled: true, Days: 7}
	if b.Trash != nil {
		trash = *b.Trash
	}
	limits := store.Limits{}
	if b.Limits != nil {
		limits = *b.Limits
	}
	relay := store.RelaySettings{}
	if b.Relay != nil {
		relay = *b.Relay
	}
	return store.APIKey{
		DefaultDrive: b.DefaultDrive,
		ExpiresAt:    b.ExpiresAt,
		Drives:       b.Drives,
		GlobalPerms:  b.GlobalPerms,
		Limits:       limits,
		Trash:        trash,
		Relay:        relay,
	}
}

// onboardingPrompt builds the §4.6 "use this key" text.
func (p *Portal) onboardingPrompt(k store.APIKey, plaintext string, r *http.Request) string {
	base := p.publicBaseURL(r)
	driveParts := make([]string, 0, len(k.Drives))
	for _, g := range k.Drives {
		mode := "read-only"
		perms := map[string]bool{}
		for _, pr := range g.Perms {
			perms[pr] = true
		}
		if perms["write"] {
			mode = "read/write"
		} else if perms["list"] && len(g.Perms) > 1 {
			mode = "read"
		} else if perms["list"] {
			mode = "list-only"
		}
		root := g.Root
		if root == "" {
			root = "/"
		}
		part := g.Drive + " (" + mode
		if root != "/" {
			part += ", root " + root
		}
		driveParts = append(driveParts, part+")")
	}
	policy := strings.Join(strings.Fields(strings.ReplaceAll(p.policyText(), "\n", " ")), " ")
	lines := []string{
		"The cloud2api netdisk gateway is already deployed. Connect this agent/project using its basic file API and only the operations this key permits.",
		"",
		"Base URL: " + base,
		"API key: " + plaintext,
		"Authentication: apikey HTTP header",
		"",
		"First call GET /v1/me to inspect the effective drives, roots and permissions; do not write a test file merely to connect.",
		"Use explicit drive IDs for file operations. A listed operation is not a grant; do not attempt deletion without delete permission.",
		"",
		"Fetch and follow the basic integration instructions:",
		base + "/custom/agent-instructions.txt",
		"",
		"Basic OpenAPI:",
		base + "/apidocumentation/openapi.json",
		"",
		"Drives: " + strings.Join(driveParts, ", "),
		"Policy: " + policy,
		"",
		"Download the helper " + base + "/custom/c2a.py and the skill " + base + "/custom/SKILL.md if your environment supports them.",
	}
	return strings.Join(lines, "\n")
}

func (p *Portal) policyText() string {
	if v, ok := p.st.GetSetting("policy_text"); ok && strings.TrimSpace(v) != "" {
		return v
	}
	return "max upload 8 GiB per file; /.c2a-trash trash retention 7 days by default; other paths are durable netdisk storage — do not delete files you did not create. Traffic passes through the gateway host."
}

// publicBaseURL resolves the base URL for prompts: public_base_url setting,
// else the request origin.
func (p *Portal) publicBaseURL(r *http.Request) string {
	if v, ok := p.st.GetSetting("public_base_url"); ok && v != "" {
		return strings.TrimRight(v, "/")
	}
	scheme := "http"
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

// randomHex returns n random hex characters (id generation for unnamed keys).
func randomHex(n int) string {
	buf := make([]byte, (n+1)/2)
	if _, err := rand.Read(buf); err != nil {
		return "0000"
	}
	return hex.EncodeToString(buf)[:n]
}
