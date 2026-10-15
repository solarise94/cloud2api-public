package admin

import (
	"net/http"
	"strconv"
)

func (p *Portal) serveConnectReturn(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", relayCSP)
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if r.Method != http.MethodGet {
		respondMethodNotAllowed(w, http.MethodGet)
		return
	}
	values := r.URL.Query()
	if len(values) != 0 {
		ids := values["storage_id"]
		if len(values) != 1 || len(ids) != 1 {
			respondError(w, http.StatusBadRequest, "缺少唯一的 storage_id")
			return
		}
		id, err := strconv.ParseInt(ids[0], 10, 64)
		if err != nil || id <= 0 || strconv.FormatInt(id, 10) != ids[0] {
			respondError(w, http.StatusBadRequest, "storage_id 必须是正整数")
			return
		}
	}
	page, err := webFS.ReadFile("web/connect-return.html")
	if err != nil {
		respondError(w, http.StatusInternalServerError, "返回页面暂不可用")
		return
	}
	serveHTML(w, http.StatusOK, page)
}
