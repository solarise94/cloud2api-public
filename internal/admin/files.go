package admin

import (
	"mime"
	"net/http"
	"path"
	"strings"
)

// workspaceIDPattern describes a legal workspace entry id: at least 128 bit
// of randomness in the URL-safe alphabet (DESIGN §2 — the id is a
// non-secret bookmark, never a credential). Anything else (path fragments,
// dots, user-chosen short ids) is not routed.
const workspaceIDAlphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_"

const (
	workspaceIDMinLen = 22 // base64url of 16 bytes
	workspaceIDMaxLen = 64
)

// validWorkspaceID reports whether id is a well-formed random entry id.
func validWorkspaceID(id string) bool {
	if len(id) < workspaceIDMinLen || len(id) > workspaceIDMaxLen {
		return false
	}
	for i := 0; i < len(id); i++ {
		if !strings.ContainsRune(workspaceIDAlphabet, rune(id[i])) {
			return false
		}
	}
	return true
}

// workspaceAPIPath reports whether a /files path belongs to the workspace
// API (/files/api/* and /files/w/<id>/api/*, DESIGN §2). Those requests are
// delegated verbatim to the installed workspace handler; everything else is
// the portal's static public shell.
func workspaceAPIPath(urlPath string) bool {
	rest := strings.TrimPrefix(urlPath, "/files")
	if rest == "/api" || strings.HasPrefix(rest, "/api/") {
		return true
	}
	if !strings.HasPrefix(rest, "/w/") {
		return false
	}
	segments := strings.SplitN(strings.TrimPrefix(rest, "/"), "/", 3)
	if len(segments) != 3 {
		return false
	}
	return segments[2] == "api" || strings.HasPrefix(segments[2], "api/")
}

// serveFiles splits /files traffic between the backend workspace API handler
// (installed via SetWorkspaceHandler; API requests are delegated verbatim)
// and the static public shells, which never touch credentials or sessions:
// the shell is identical for every entry id and carries no data.
func (p *Portal) serveFiles(w http.ResponseWriter, r *http.Request) {
	if workspaceAPIPath(r.URL.Path) {
		h := p.workspaceHandler()
		if h == nil {
			// Fail closed: without the handler there is no workspace API —
			// never fall through to the data plane.
			w.Header().Set("Cache-Control", "no-store")
			respondError(w, http.StatusServiceUnavailable, "file workspace API not installed")
			return
		}
		h.ServeHTTP(w, r)
		return
	}
	p.serveFilesStatic(w, r)
}

// serveFilesStatic serves the public shells: the workspace list page, the
// per-entry bookmark page /files/w/<id>/ and the whitelisted assets.
func (p *Portal) serveFilesStatic(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", relayCSP)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Cross-Origin-Resource-Policy", "same-origin")
	if r.Method != http.MethodGet {
		respondMethodNotAllowed(w, http.MethodGet)
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/files")
	switch {
	case rest == "" || rest == "/":
		data, err := webFS.ReadFile("web/files.html")
		if err != nil {
			respondError(w, http.StatusNotFound, "file workspace not installed")
			return
		}
		serveHTML(w, http.StatusOK, data)
	case strings.HasPrefix(rest, "/assets/"):
		p.serveFilesAsset(w, strings.TrimPrefix(rest, "/assets/"))
	case strings.HasPrefix(rest, "/w/"):
		// /files/w/<id>/ — the same static shell for every entry id. The
		// id is a non-secret bookmark: a malformed id is simply a 404, and
		// no static subpath exists below it (API paths were delegated
		// above).
		tail := strings.TrimPrefix(rest, "/w/")
		id, extra := tail, ""
		if slash := strings.IndexByte(tail, '/'); slash >= 0 {
			id, extra = tail[:slash], tail[slash:]
		}
		if extra != "" && extra != "/" {
			respondError(w, http.StatusNotFound, "not found")
			return
		}
		if !validWorkspaceID(id) {
			respondError(w, http.StatusNotFound, "not found")
			return
		}
		data, err := webFS.ReadFile("web/files.html")
		if err != nil {
			respondError(w, http.StatusNotFound, "file workspace not installed")
			return
		}
		serveHTML(w, http.StatusOK, data)
	default:
		respondError(w, http.StatusNotFound, "not found")
	}
}

// filesAssetWhitelist is the only static asset the public file shell may
// load; everything else 404s.
var filesAssetWhitelist = map[string]bool{
	"app.css": true, "files.css": true, "files.js": true, "logo.png": true,
}

func (p *Portal) serveFilesAsset(w http.ResponseWriter, name string) {
	if strings.Contains(name, "..") || strings.ContainsAny(name, "/\\\x00") {
		respondError(w, http.StatusBadRequest, "invalid asset path")
		return
	}
	if !filesAssetWhitelist[name] {
		respondError(w, http.StatusNotFound, "asset not found")
		return
	}
	data, err := webFS.ReadFile("web/assets/" + name)
	if err != nil {
		respondError(w, http.StatusNotFound, "asset not found")
		return
	}
	w.Header().Set("Content-Type", mime.TypeByExtension(path.Ext(name)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}
