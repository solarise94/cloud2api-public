package admin

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"cloud2api/internal/openlist"
)

func handoffOrigin(r *http.Request) (*url.URL, bool) {
	u, err := url.Parse(r.Header.Get("Origin"))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" ||
		u.User != nil || u.Path != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" ||
		!strings.EqualFold(u.Host, r.Host) || (r.TLS != nil && u.Scheme != "https") {
		return nil, false
	}
	return u, true
}

func validConnectTarget(target string, info openlist.ConnectInfo) bool {
	if target == "/" {
		return true
	}
	if strings.HasPrefix(target, "/reauth/") {
		raw := strings.TrimPrefix(target, "/reauth/")
		n, err := strconv.ParseInt(raw, 10, 64)
		return err == nil && n > 0 && strconv.FormatInt(n, 10) == raw
	}
	for _, prefix := range []string{"/new/", "/app/"} {
		if strings.HasPrefix(target, prefix) {
			id := strings.TrimPrefix(target, prefix)
			for _, platform := range info.Platforms {
				if id != "" && id == platform.ID {
					return true
				}
			}
		}
	}
	return false
}

func (p *Portal) handleConnectHandoff(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	origin, ok := handoffOrigin(r)
	if !ok {
		respondError(w, http.StatusForbidden, "免登录入口需要同源 Origin")
		return
	}
	if !jsonContentType(r) {
		respondError(w, http.StatusUnsupportedMediaType, "免登录入口需要 application/json")
		return
	}
	var body struct {
		Target string `json:"target"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		respondError(w, http.StatusBadRequest, "免登录请求只接受 target")
		return
	}
	if dec.Decode(new(any)) != io.EOF {
		respondError(w, http.StatusBadRequest, "免登录请求不是有效的单个 JSON 对象")
		return
	}
	if body.Target == "" {
		body.Target = "/"
	}
	base := p.openlistBaseURL()
	if base == "" {
		respondError(w, http.StatusServiceUnavailable, "请先设置有效的 OpenList 浏览器访问地址")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), defaultAdminCallTimeout)
	defer cancel()
	info, available, _ := p.connectState(ctx, false)
	if !validConnectTarget(body.Target, info) {
		respondError(w, http.StatusBadRequest, "未知的网盘连接目标")
		return
	}
	link, _ := url.Parse(openlistConnectURL(base))
	query := link.Query()
	query.Set("return", origin.String()+"/admin/connect-return?storage_id={storage_id}")
	link.RawQuery = query.Encode()
	link.Fragment = body.Target
	fallback := link.String()
	if !available || !info.Handoff {
		respondData(w, map[string]any{"url": "", "fallback_url": fallback})
		return
	}
	ticket, err := p.client.StartConnectHandoff(ctx)
	if err != nil {
		respondJSON(w, http.StatusBadGateway, map[string]any{
			"code": http.StatusBadGateway, "message": "OpenList 免登录入口暂不可用，可使用手动登录入口；不要输入 cloud2api 密码",
			"data": map[string]any{"fallback_url": fallback},
		})
		return
	}
	fragment := url.Values{}
	fragment.Set("handoff", ticket.Code)
	fragment.Set("target", body.Target)
	link.Fragment = "handoff=" + ticket.Code + "&target=" + body.Target
	link.RawFragment = fragment.Encode()
	respondData(w, map[string]any{
		"url": link.String(), "fallback_url": fallback, "expires_in": ticket.ExpiresIn,
	})
}
