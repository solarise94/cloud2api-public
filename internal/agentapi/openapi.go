package agentapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
)

// paramSpec documents one request parameter in the OpenAPI document.
type paramSpec struct {
	Name     string `json:"name"`
	In       string `json:"in"` // query | path | header
	Required bool   `json:"required"`
	Desc     string `json:"description"`
}

// routeSpec documents one endpoint. The same table drives the OpenAPI
// document and its coverage test; the dispatch code lives in server.go and
// must stay in sync (tested by TestOpenAPICoversV1Routes).
type routeSpec struct {
	Method  string
	Path    string // pattern with {vars}
	Summary string
	Auth    string // "apikey" | "none"
	Params  []paramSpec
}

func qp(name string, required bool, desc string) paramSpec {
	return paramSpec{Name: name, In: "query", Required: required, Desc: desc}
}

func hp(name string, required bool, desc string) paramSpec {
	return paramSpec{Name: name, In: "header", Required: required, Desc: desc}
}

// routeTable is every public endpoint of the agent API surface.
var routeTable = []routeSpec{
	{Method: http.MethodGet, Path: "/v1", Summary: "Entry index: quickstart and asset URLs", Auth: "apikey"},
	{Method: http.MethodGet, Path: "/v1/me", Summary: "This key's drives, permissions, limits, trash, relay hop capability (relay_hop_limit) and policy", Auth: "apikey"},
	{Method: http.MethodGet, Path: "/v1/drives", Summary: "Drives granted to this key (with health)", Auth: "apikey"},

	{Method: http.MethodGet, Path: "/v1/drives/{drive}/list", Summary: "List a directory page", Auth: "apikey",
		Params: []paramSpec{
			{Name: "drive", In: "path", Required: true, Desc: "drive id"},
			qp("path", false, "logical directory path inside the key root (default /)"),
			qp("page", false, "1-based page number (default 1)"),
			qp("per_page", false, "entries per page (default 200, max 1000)"),
		}},
	{Method: http.MethodGet, Path: "/v1/drives/{drive}/stat", Summary: "Metadata of one path; includes recorded upload checksums when size+mtime are unchanged", Auth: "apikey",
		Params: []paramSpec{{Name: "drive", In: "path", Required: true, Desc: "drive id"}, qp("path", true, "logical path")}},
	{Method: http.MethodGet, Path: "/v1/drives/{drive}/files", Summary: "Stream a file (Range supported); X-C2A-SHA256/X-C2A-CRC64 set when an upload record matches", Auth: "apikey",
		Params: []paramSpec{{Name: "drive", In: "path", Required: true, Desc: "drive id"}, qp("path", true, "logical file path")}},
	{Method: http.MethodPut, Path: "/v1/drives/{drive}/files", Summary: "Upload raw body (Content-Length required) or multipart `file` part; CRC64 (CRC-64/XZ) and SHA-256 computed server-side", Auth: "apikey",
		Params: []paramSpec{
			{Name: "drive", In: "path", Required: true, Desc: "drive id"},
			qp("path", true, "logical target path"),
			qp("on_conflict", false, "fail (default) | overwrite | rename"),
			hp("Idempotency-Key", false, "replay-safe retry within 24h"),
			hp("X-Expected-CRC64", false, "decimal CRC-64/XZ; mismatch deletes the upload and returns 422"),
			hp("X-Expected-SHA256", false, "hex sha256; mismatch deletes the upload and returns 422"),
		}},
	{Method: http.MethodPost, Path: "/v1/drives/{drive}/mkdir", Summary: "Create a directory recursively (idempotent)", Auth: "apikey",
		Params: []paramSpec{{Name: "drive", In: "path", Required: true, Desc: "drive id"}, hp("Idempotency-Key", false, "replay-safe retry within 24h")}},
	{Method: http.MethodPost, Path: "/v1/drives/{drive}/delete", Summary: "Delete paths (into /.c2a-trash when the key has trash enabled); missing paths answer already_absent", Auth: "apikey",
		Params: []paramSpec{{Name: "drive", In: "path", Required: true, Desc: "drive id"}, hp("Idempotency-Key", false, "replay-safe retry within 24h")}},
	{Method: http.MethodPost, Path: "/v1/drives/{drive}/move", Summary: "Move or rename within the drive", Auth: "apikey",
		Params: []paramSpec{{Name: "drive", In: "path", Required: true, Desc: "drive id"}, hp("Idempotency-Key", false, "replay-safe retry within 24h")}},

	// Default-drive aliases of the operations above.
	{Method: http.MethodGet, Path: "/v1/list", Summary: "list via the key's default drive", Auth: "apikey",
		Params: []paramSpec{qp("path", false, "logical directory path (default /)"), qp("page", false, "page"), qp("per_page", false, "page size")}},
	{Method: http.MethodGet, Path: "/v1/stat", Summary: "stat via the key's default drive", Auth: "apikey", Params: []paramSpec{qp("path", true, "logical path")}},
	{Method: http.MethodGet, Path: "/v1/files", Summary: "download via the key's default drive", Auth: "apikey", Params: []paramSpec{qp("path", true, "logical file path")}},
	{Method: http.MethodPut, Path: "/v1/files", Summary: "upload via the key's default drive", Auth: "apikey",
		Params: []paramSpec{qp("path", true, "logical target path"), qp("on_conflict", false, "fail|overwrite|rename"), hp("Idempotency-Key", false, "replay-safe retry"), hp("X-Expected-CRC64", false, "expected CRC-64/XZ decimal"), hp("X-Expected-SHA256", false, "expected sha256 hex")}},
	{Method: http.MethodPost, Path: "/v1/mkdir", Summary: "mkdir via the key's default drive", Auth: "apikey"},
	{Method: http.MethodPost, Path: "/v1/delete", Summary: "delete via the key's default drive", Auth: "apikey"},
	{Method: http.MethodPost, Path: "/v1/move", Summary: "move via the key's default drive", Auth: "apikey"},

	{Method: http.MethodGet, Path: "/v1/trash", Summary: "This key's trash items", Auth: "apikey"},
	{Method: http.MethodPost, Path: "/v1/trash/restore", Summary: "Restore one trash item to its original path", Auth: "apikey"},
	{Method: http.MethodPost, Path: "/v1/trash/purge", Summary: "Permanently remove one trash item (purge permission)", Auth: "apikey"},

	// ---- Phase 1b: agent autonomy (UPGRADE §4.10) ----
	{Method: http.MethodPost, Path: "/v1/selftest", Summary: "Write+read+CRC64+delete roundtrip on every writable drive; records last_selftest_at", Auth: "apikey"},
	{Method: http.MethodGet, Path: "/v1/tree", Summary: "Directory tree of one path (bounded by depth and max_entries)",
		Params: []paramSpec{qp("path", false, "logical directory path (default /)"), qp("depth", false, "levels to descend (default 3, max 10)"), qp("max_entries", false, "entry cap (default 2000, max 10000)")}},
	{Method: http.MethodGet, Path: "/v1/search", Summary: "Case-insensitive substring search (OpenList index when enabled, else bounded walk)",
		Params: []paramSpec{qp("q", true, "search keywords"), qp("path", false, "logical root of the search (default /)")}},
	{Method: http.MethodGet, Path: "/v1/drives/{drive}/tree", Summary: "Directory tree on one drive (bounded by depth and max_entries)",
		Params: []paramSpec{{Name: "drive", In: "path", Required: true, Desc: "drive id"},
			qp("path", false, "logical directory path (default /)"), qp("depth", false, "levels to descend (default 3, max 10)"), qp("max_entries", false, "entry cap (default 2000, max 10000)")}},
	{Method: http.MethodGet, Path: "/v1/drives/{drive}/search", Summary: "Case-insensitive substring search on one drive (OpenList index when enabled, else bounded walk)",
		Params: []paramSpec{{Name: "drive", In: "path", Required: true, Desc: "drive id"},
			qp("q", true, "search keywords"), qp("path", false, "logical root of the search (default /)")}},
	{Method: http.MethodPost, Path: "/v1/transfers", Summary: "Cross-drive (or same-drive) copy/move as a job; answers 202 {job}",
		Params: []paramSpec{hp("Idempotency-Key", false, "replay-safe retry within 24h")}},
	{Method: http.MethodPost, Path: "/v1/fetch", Summary: "Download a public URL into a drive via OpenList offline download (SimpleHttp); answers 202 {job}",
		Params: []paramSpec{hp("Idempotency-Key", false, "replay-safe retry within 24h")}},
	{Method: http.MethodPost, Path: "/v1/batch", Summary: "Up to 100 mkdir/delete/move/copy ops; cross-drive copies turn the batch into a job (202)",
		Params: []paramSpec{hp("Idempotency-Key", false, "replay-safe retry within 24h")}},
	{Method: http.MethodGet, Path: "/v1/jobs", Summary: "List this key's jobs (filter by state)",
		Params: []paramSpec{qp("state", false, "queued|running|succeeded|failed|canceled"), qp("limit", false, "default 50, max 200")}},
	{Method: http.MethodGet, Path: "/v1/jobs/{id}", Summary: "One job; ?wait=N long-polls up to 60s and returns early on state change",
		Params: []paramSpec{{Name: "id", In: "path", Required: true, Desc: "job id"}, qp("wait", false, "seconds to wait for a state change (max 60)")}},
	{Method: http.MethodPost, Path: "/v1/jobs/{id}/cancel", Summary: "Cancel a queued/running job (OpenList tasks are canceled where possible)",
		Params: []paramSpec{{Name: "id", In: "path", Required: true, Desc: "job id"}}},
	{Method: http.MethodGet, Path: "/v1/keys", Summary: "List this key's child keys (delegate permission)"},
	{Method: http.MethodPost, Path: "/v1/keys", Summary: "Create a child key that must stay a subset of this key (delegate permission); TTL default 24h, max 7d"},
	{Method: http.MethodDelete, Path: "/v1/keys/{id}", Summary: "Revoke one of this key's descendants (delegate permission)",
		Params: []paramSpec{{Name: "id", In: "path", Required: true, Desc: "child key id"}}},
	{Method: http.MethodGet, Path: "/v1/activity", Summary: "This key's recent audit entries (logical paths only)",
		Params: []paramSpec{qp("limit", false, "default 50, max 200")}},

	{Method: http.MethodPost, Path: "/v1/links", Summary: "Create a limited-time direct link (max 7 days)", Auth: "apikey",
		Params: []paramSpec{hp("Idempotency-Key", false, "replay-safe retry within 24h")}},

	{Method: http.MethodGet, Path: "/l/{token}", Summary: "Download via a limited-time link (no API key); expired/revoked answers 410", Auth: "none"},

	// ---- Gokapi compatibility layer (UPGRADE §4.7, tag "gokapi-compat") ----
	{Method: http.MethodGet, Path: "/api/info/version", Summary: "Gokapi-compat: server version", Auth: "apikey"},
	{Method: http.MethodGet, Path: "/api/info/config", Summary: "Gokapi-compat: upload parameters (MiB; relay permission)", Auth: "apikey"},
	{Method: http.MethodPost, Path: "/api/files/add", Summary: "Gokapi-compat: multipart upload (file, allowedDownloads, expiryDays); returns UploadResult with CRC64 decimal string plus UploadId/Status receipt fields", Auth: "apikey",
		Params: []paramSpec{
			hp("password", false, "rejected with 400 when non-empty: the relay does not support passwords"),
			hp("X-Upload-ID", false, "client-chosen random upload id; the immutable identity of this one attempt — a reused id answers 409, retries need a fresh id"),
		}},
	{Method: http.MethodGet, Path: "/api/files/list", Summary: "Gokapi-compat: this key's relay files (array, or null when empty)", Auth: "apikey"},
	{Method: http.MethodGet, Path: "/api/files/list/{id}", Summary: "Gokapi-compat: one relay file's metadata",
		Params: []paramSpec{{Name: "id", In: "path", Required: true, Desc: "file id"}}},
	{Method: http.MethodDelete, Path: "/api/files/delete", Summary: "Gokapi-compat: delete now, or arm a cancellable delayed deletion", Auth: "apikey",
		Params: []paramSpec{
			hp("id", true, "the file id to delete"),
			hp("delay", false, "seconds to wait before deleting (cancellable via /api/files/restore)"),
		}},
	{Method: http.MethodPost, Path: "/api/files/restore", Summary: "Gokapi-compat: cancel a pending delayed deletion", Auth: "apikey",
		Params: []paramSpec{hp("id", true, "the file id to restore")}},
	{Method: http.MethodGet, Path: "/api/files/download/{id}", Summary: "Gokapi-compat: authenticated file download",
		Params: []paramSpec{
			{Name: "id", In: "path", Required: true, Desc: "file id"},
			hp("increaseCounter", false, "consume one download when true"),
			hp("presignUrl", false, "not supported; answers 400 when true"),
		}},
	{Method: http.MethodGet, Path: "/api/files/status/{uploadId}", Summary: "Gokapi-compat additive: one upload receipt of this key (uploading|completed|failed|interrupted); unknown or foreign ids answer 404",
		Params: []paramSpec{{Name: "uploadId", In: "path", Required: true, Desc: "the upload id returned by /api/files/add or /api/chunk/complete"}}},
	{Method: http.MethodPost, Path: "/api/chunk/add", Summary: "Gokapi-compat: stage one chunk (multipart file, uuid, filesize, offset; >=5 MiB except the last); staging acknowledgement, NOT upload completion", Auth: "apikey",
		Params: []paramSpec{hp("X-Upload-ID", false, "optional client upload id; repeated staging may reuse it (metadata only, outcome never reset)")}},
	{Method: http.MethodPost, Path: "/api/chunk/complete", Summary: "Gokapi-compat: assemble staged chunks into a relay file; returns UploadId/Status (nonblocking=true answers Status=uploading and finishes in the background; poll /api/files/status/{uploadId}). A duplicate completion of a finished attempt answers 409", Auth: "apikey",
		Params: []paramSpec{
			hp("uuid", true, "the chunk upload uuid"),
			hp("filename", true, "final filename ('base64:...' allowed)"),
			hp("filesize", true, "total size in bytes"),
			hp("contenttype", false, "MIME type"),
			hp("allowedDownloads", false, "default 1; 0 = unlimited"),
			hp("expiryDays", false, "capped at the key's relay max age; 0 = max"),
			hp("nonblocking", false, "true: acknowledge immediately and finish in the background"),
			hp("X-Upload-ID", false, "client-chosen random upload id; adopts the receipt staged with it — a finished attempt answers 409 and needs a fresh id"),
		}},
	{Method: http.MethodGet, Path: "/downloadFile", Summary: "Gokapi-compat: public raw download by id (consumes one download)", Auth: "none",
		Params: []paramSpec{qp("id", true, "file id")}},
	{Method: http.MethodGet, Path: "/d", Summary: "Gokapi-compat: public HTML share page (UrlDownload)", Auth: "none",
		Params: []paramSpec{qp("id", true, "file id")}},
	{Method: http.MethodGet, Path: "/h/{hotlinkId}", Summary: "Gokapi-compat: public raw download by hotlink id (UrlHotlink; consumes one download)", Auth: "none",
		Params: []paramSpec{{Name: "hotlinkId", In: "path", Required: true, Desc: "hotlink id"}}},
	{Method: http.MethodGet, Path: "/custom/gokapi-agent-instructions.txt", Summary: "Gokapi-style onboarding instructions rendered with this gateway's base URL and policy", Auth: "none"},

	{Method: http.MethodGet, Path: "/healthz", Summary: "Liveness", Auth: "none"},
	{Method: http.MethodGet, Path: "/readyz", Summary: "Readiness (OpenList reachable, admin token valid)", Auth: "none"},
	{Method: http.MethodGet, Path: "/custom/agent-instructions.txt", Summary: "Per-request rendered agent onboarding instructions", Auth: "none"},
	{Method: http.MethodGet, Path: "/custom/c2a.py", Summary: "Stdlib-only Python CLI helper", Auth: "none"},
	{Method: http.MethodGet, Path: "/custom/SKILL.md", Summary: "Skill description for agent skill directories", Auth: "none"},
	{Method: http.MethodGet, Path: "/apidocumentation/openapi.json", Summary: "This document", Auth: "none"},
}

// basicAPIDescription documents the served (basic) profile.
const basicAPIDescription = "Agent-friendly netdisk API: the basic onboarding surface. Authenticate with the `apikey` header (Bearer also accepted). Start from GET /v1/me to discover this key's drives, then list, stat, upload and download files on a granted drive, copy or move across drives with transfers (long-poll the job), and share files with limited-time links. All paths are logical paths inside the key's per-drive root; responses never contain OpenList internals."

// fullAPIDescription documents the complete protocol (project docs only).
const fullAPIDescription = "Agent-friendly netdisk API. Authenticate with the `apikey` header (Bearer also accepted). All paths are logical paths inside the key's per-drive root; responses never contain OpenList internals. Long operations run as jobs with long-polling; destructive operations support dry_run."

// isBasicRoute is the basic onboarding selector: an explicit method+path
// allowlist (12 operations, 11 paths). It fails closed — anything not listed,
// including future routeTable entries and wrong methods, is not basic.
func isBasicRoute(method, path string) bool {
	switch {
	case method == http.MethodGet && path == "/v1/me":
		return true
	case method == http.MethodGet && path == "/v1/drives/{drive}/list":
		return true
	case method == http.MethodGet && path == "/v1/drives/{drive}/stat":
		return true
	case method == http.MethodGet && path == "/v1/drives/{drive}/files":
		return true
	case method == http.MethodPut && path == "/v1/drives/{drive}/files":
		return true
	case method == http.MethodPost && path == "/v1/drives/{drive}/mkdir":
		return true
	case method == http.MethodPost && path == "/v1/drives/{drive}/delete":
		return true
	case method == http.MethodPost && path == "/v1/drives/{drive}/move":
		return true
	case method == http.MethodPost && path == "/v1/transfers":
		return true
	case method == http.MethodGet && path == "/v1/jobs/{id}":
		return true
	case method == http.MethodPost && path == "/v1/links":
		return true
	case method == http.MethodGet && path == "/l/{token}":
		return true
	default:
		return false
	}
}

// renderOpenAPI builds the OpenAPI 3.0 document for the given routes.
func renderOpenAPI(baseURL, title, description string, routes []routeSpec) map[string]any {
	paths := map[string]any{}
	for _, r := range routes {
		item, _ := paths[r.Path].(map[string]any)
		if item == nil {
			item = map[string]any{}
			paths[r.Path] = item
		}
		op := map[string]any{
			"summary": r.Summary,
			"tags":    []string{tagOf(r.Path)},
		}
		var params []paramSpec
		// {var} path parameters
		for _, seg := range splitPattern(r.Path) {
			if len(seg) > 2 && seg[0] == '{' && seg[len(seg)-1] == '}' {
				params = append(params, paramSpec{Name: seg[1 : len(seg)-1], In: "path", Required: true, Desc: "path parameter"})
			}
		}
		all := append(params, r.Params...)
		if len(all) > 0 {
			op["parameters"] = all
		}
		if len(r.Method) > 0 && r.Method != http.MethodGet && r.Method != http.MethodDelete && needsBodyDoc(r.Path) {
			ct := "application/json"
			if isMultipartRoute(r.Path) {
				ct = "multipart/form-data"
			}
			op["requestBody"] = map[string]any{
				"required": true,
				"content": map[string]any{
					ct: map[string]any{"schema": bodySchemaFor(r.Path)},
				},
			}
		}
		item[strings2lower(r.Method)] = op
	}
	return map[string]any{
		"openapi": "3.0.3",
		"info": map[string]any{
			"title":       title,
			"version":     "v0.4-1b",
			"description": description,
		},
		"servers": []map[string]string{{"url": baseURL}},
		"paths":   paths,
	}
}

// buildOpenAPI renders the served document: the isBasicRoute subset of the
// route table. The full document is never served (docs/openapi.full.json only).
func buildOpenAPI(baseURL string) map[string]any {
	routes := make([]routeSpec, 0, 12)
	for _, r := range routeTable {
		if isBasicRoute(r.Method, r.Path) {
			routes = append(routes, r)
		}
	}
	return renderOpenAPI(baseURL, "cloud2api agent gateway", basicAPIDescription, routes)
}

// buildFullOpenAPI renders every routeTable entry; internal, fixture-only.
func buildFullOpenAPI(baseURL string) map[string]any {
	return renderOpenAPI(baseURL, "cloud2api agent gateway (full protocol)", fullAPIDescription, routeTable)
}

// marshalOpenAPIDoc encodes a document exactly as the endpoint serves it.
func marshalOpenAPIDoc(doc map[string]any) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	_ = enc.Encode(doc)
	return buf.Bytes()
}

func strings2lower(m string) string {
	return map[string]string{
		"GET": "get", "PUT": "put", "POST": "post", "DELETE": "delete", "PATCH": "patch",
	}[m]
}

func splitPattern(p string) []string {
	out := []string{}
	cur := ""
	for _, r := range p {
		if r == '/' {
			if cur != "" {
				out = append(out, cur)
			}
			cur = ""
			continue
		}
		cur += string(r)
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}

func tagOf(p string) string {
	switch {
	// Gokapi compatibility layer (UPGRADE §4.7). The /api/* match must come
	// before the generic fallthrough; /api/storage and /api/transfer stay in
	// the legacy gateway and are not routed here.
	case p == "/api/info/version" || p == "/api/info/config" ||
		strings.HasPrefix(p, "/api/files/") || p == "/api/files" ||
		strings.HasPrefix(p, "/api/chunk/") ||
		p == "/downloadFile" || p == "/d" || strings.HasPrefix(p, "/h/") ||
		p == "/custom/gokapi-agent-instructions.txt":
		return "gokapi-compat"
	case p == "/v1" || p == "/v1/me" || p == "/v1/drives" || p == "/v1/activity":
		return "discovery"
	case strings.HasPrefix(p, "/v1/trash"):
		return "trash"
	case strings.HasPrefix(p, "/v1/links") || strings.HasPrefix(p, "/l/"):
		return "links"
	case strings.HasPrefix(p, "/v1/jobs") || strings.HasPrefix(p, "/v1/transfers") || strings.HasPrefix(p, "/v1/fetch") || strings.HasPrefix(p, "/v1/batch"):
		return "jobs"
	case strings.HasPrefix(p, "/v1/keys"):
		return "keys"
	case strings.HasPrefix(p, "/v1/selftest"):
		return "selftest"
	case strings.HasPrefix(p, "/v1/tree") || strings.HasPrefix(p, "/v1/search") ||
		p == "/v1/drives/{drive}/tree" || p == "/v1/drives/{drive}/search":
		return "browse"
	case strings.HasPrefix(p, "/custom/") || strings.HasPrefix(p, "/apidocumentation"):
		return "assets"
	case p == "/healthz" || p == "/readyz":
		return "health"
	default:
		return "files"
	}
}

func needsBodyDoc(p string) bool {
	switch p {
	case "/v1/drives/{drive}/mkdir", "/v1/drives/{drive}/delete", "/v1/drives/{drive}/move",
		"/v1/mkdir", "/v1/delete", "/v1/move", "/v1/trash/restore", "/v1/trash/purge", "/v1/links",
		"/v1/transfers", "/v1/fetch", "/v1/batch", "/v1/keys",
		"/api/files/add", "/api/chunk/add":
		return true
	}
	return false
}

// isMultipartRoute marks the multipart endpoints of the Gokapi compat layer.
func isMultipartRoute(p string) bool {
	return p == "/api/files/add" || p == "/api/chunk/add"
}

func bodySchemaFor(p string) map[string]any {
	switch p {
	case "/v1/drives/{drive}/mkdir", "/v1/mkdir":
		return map[string]any{"type": "object", "required": []string{"path"}, "properties": map[string]any{"path": map[string]any{"type": "string"}}}
	case "/v1/drives/{drive}/delete", "/v1/delete":
		return map[string]any{"type": "object", "required": []string{"paths"}, "properties": map[string]any{"paths": map[string]any{"type": "array", "items": map[string]any{"type": "string"}}}}
	case "/v1/drives/{drive}/move", "/v1/move":
		return map[string]any{"type": "object", "required": []string{"from", "to"}, "properties": map[string]any{
			"from": map[string]any{"type": "string"}, "to": map[string]any{"type": "string"},
			"on_conflict": map[string]any{"type": "string", "enum": []string{"fail", "overwrite", "rename"}}}}
	case "/v1/trash/restore", "/v1/trash/purge":
		return map[string]any{"type": "object", "required": []string{"id"}, "properties": map[string]any{"id": map[string]any{"type": "integer"}}}
	case "/v1/transfers":
		return map[string]any{"type": "object", "required": []string{"from", "to"}, "properties": map[string]any{
			"from":        drivePathSchema("source"),
			"to":          drivePathSchema("destination"),
			"mode":        map[string]any{"type": "string", "enum": []string{"copy", "move"}, "default": "copy"},
			"on_conflict": map[string]any{"type": "string", "enum": []string{"fail", "overwrite", "rename"}, "default": "fail"},
			"dry_run":     map[string]any{"type": "boolean", "description": "return would_affect {files,dirs,bytes} and the first 200 items without doing anything"}}}
	case "/v1/fetch":
		return map[string]any{"type": "object", "required": []string{"url", "path"}, "properties": map[string]any{
			"url":         map[string]any{"type": "string", "description": "absolute http(s) URL"},
			"drive":       map[string]any{"type": "string"},
			"path":        map[string]any{"type": "string", "description": "target file path inside the drive"},
			"on_conflict": map[string]any{"type": "string", "enum": []string{"fail", "overwrite", "rename"}, "default": "fail"},
			"dry_run":     map[string]any{"type": "boolean"}}}
	case "/v1/batch":
		return map[string]any{"type": "object", "required": []string{"ops"}, "properties": map[string]any{
			"ops": map[string]any{"type": "array", "maxItems": 100, "items": map[string]any{"type": "object", "required": []string{"op"}, "properties": map[string]any{
				"op":          map[string]any{"type": "string", "enum": []string{"mkdir", "delete", "move", "copy"}},
				"drive":       map[string]any{"type": "string"},
				"path":        map[string]any{"type": "string"},
				"paths":       map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
				"from":        drivePathSchema("move/copy source"),
				"to":          drivePathSchema("move/copy destination"),
				"on_conflict": map[string]any{"type": "string", "enum": []string{"fail", "overwrite", "rename"}}}}},
			"dry_run": map[string]any{"type": "boolean"}}}
	case "/v1/links":
		return map[string]any{"type": "object", "properties": map[string]any{
			"drive": map[string]any{"type": "string"}, "path": map[string]any{"type": "string"},
			"expires_in": map[string]any{"type": "integer", "description": "seconds, default 3600, max 604800"}}}
	case "/v1/keys":
		return map[string]any{"type": "object", "required": []string{"drives"}, "properties": map[string]any{
			"id":           map[string]any{"type": "string", "description": "optional child key id"},
			"drives":       map[string]any{"type": "array", "items": map[string]any{"type": "object", "properties": map[string]any{"drive": map[string]any{"type": "string"}, "root": map[string]any{"type": "string"}, "perms": map[string]any{"type": "array", "items": map[string]any{"type": "string"}}}}},
			"global_perms": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
			"ttl_seconds":  map[string]any{"type": "integer", "description": "default 86400, max 604800"}}}
	case "/api/files/add":
		return map[string]any{"type": "object", "required": []string{"file"}, "properties": map[string]any{
			"file":             map[string]any{"type": "string", "format": "binary"},
			"allowedDownloads": map[string]any{"type": "integer", "description": "default 1; 0 = unlimited"},
			"expiryDays":       map[string]any{"type": "integer", "description": "capped at the key's relay max age (<= 7); 0 = max"},
			"password":         map[string]any{"type": "string", "description": "rejected with 400 when non-empty"}}}
	case "/api/chunk/add":
		return map[string]any{"type": "object", "required": []string{"file", "uuid", "filesize", "offset"}, "properties": map[string]any{
			"file":     map[string]any{"type": "string", "format": "binary"},
			"uuid":     map[string]any{"type": "string", "description": ">= 10 characters, same for all chunks"},
			"filesize": map[string]any{"type": "integer", "description": "total file size in bytes"},
			"offset":   map[string]any{"type": "integer", "description": "chunk offset from the beginning of the file"}}}
	}
	return map[string]any{"type": "object"}
}

func drivePathSchema(desc string) map[string]any {
	return map[string]any{"type": "object", "description": desc, "properties": map[string]any{
		"drive": map[string]any{"type": "string"}, "path": map[string]any{"type": "string"}}}
}

func (s *Server) handleOpenAPI(w http.ResponseWriter, r *http.Request) {
	doc := buildOpenAPI(s.baseURL(r))
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(marshalOpenAPIDoc(doc))
}
