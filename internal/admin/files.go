package admin

import (
	"mime"
	"net/http"
	"path"
	"strings"
)

// serveFiles only serves a static shell. No administrator session or upstream
// credential is exchanged for file access; the browser calls the key data plane.
func (p *Portal) serveFiles(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", relayCSP)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Cross-Origin-Resource-Policy", "same-origin")
	if r.Method != http.MethodGet {
		respondMethodNotAllowed(w, http.MethodGet)
		return
	}
	if r.URL.Path == "/files" || r.URL.Path == "/files/" {
		data, err := webFS.ReadFile("web/files.html")
		if err != nil {
			respondError(w, http.StatusNotFound, "file workspace not installed")
			return
		}
		serveHTML(w, http.StatusOK, data)
		return
	}
	if !strings.HasPrefix(r.URL.Path, "/files/assets/") {
		respondError(w, http.StatusNotFound, "not found")
		return
	}
	name := strings.TrimPrefix(r.URL.Path, "/files/assets/")
	if strings.Contains(name, "..") || strings.ContainsAny(name, "/\\\x00") {
		respondError(w, http.StatusBadRequest, "invalid asset path")
		return
	}
	switch name {
	case "app.css", "files.css", "files.js", "logo.png":
	default:
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
