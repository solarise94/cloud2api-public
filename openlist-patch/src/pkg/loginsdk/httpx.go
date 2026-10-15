package loginsdk

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// HTTPClient performs every provider request made by adapters. Tests point it
// (together with the adapters' endpoint variables) at httptest servers.
var HTTPClient = &http.Client{
	Timeout: 20 * time.Second,
	Transport: &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		TLSHandshakeTimeout: 10 * time.Second,
		IdleConnTimeout:     30 * time.Second,
	},
}

// maxResponse bounds every provider response body.
const maxResponse = 1 << 20

// Request is one provider call. At most one of Form and JSON is set.
type Request struct {
	Method string
	URL    string
	Query  url.Values
	Form   url.Values // application/x-www-form-urlencoded body
	JSON   any        // application/json body
	Header http.Header
}

// Do performs r and returns the HTTP status and body. A transport failure is
// an *Error with CodeNetwork; any HTTP status is returned to the caller.
func Do(ctx context.Context, r Request) (int, []byte, error) {
	u := r.URL
	if len(r.Query) > 0 {
		sep := "?"
		if strings.Contains(u, "?") {
			sep = "&"
		}
		u += sep + r.Query.Encode()
	}
	var body io.Reader
	contentType := ""
	switch {
	case r.Form != nil:
		body = strings.NewReader(r.Form.Encode())
		contentType = "application/x-www-form-urlencoded"
	case r.JSON != nil:
		data, err := json.Marshal(r.JSON)
		if err != nil {
			return 0, nil, Wrap(CodeInternal, "请求编码失败", err)
		}
		body = bytes.NewReader(data)
		contentType = "application/json"
	}
	method := r.Method
	if method == "" {
		method = http.MethodGet
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return 0, nil, Wrap(CodeInternal, "请求地址无效", err)
	}
	for k, vs := range r.Header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	if contentType != "" && req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", contentType)
	}
	if req.Header.Get("Accept") == "" {
		req.Header.Set("Accept", "application/json")
	}
	resp, err := HTTPClient.Do(req)
	if err != nil {
		// The URL may carry a query with secrets (e.g. client_secret on a GET
		// token endpoint); report only the host.
		host := req.URL.Host
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return 0, nil, Wrap(CodeNetwork, "无法连接 "+host, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse))
	if err != nil {
		return resp.StatusCode, nil, Wrap(CodeNetwork, "读取 "+req.URL.Host+" 的响应失败", err)
	}
	return resp.StatusCode, data, nil
}

// DoJSON performs r and decodes the body into out (when out is non-nil and
// the body is non-empty). A body that is not JSON is an *Error with
// CodeProvider carrying the HTTP status.
func DoJSON(ctx context.Context, r Request, out any) (int, []byte, error) {
	status, data, err := Do(ctx, r)
	if err != nil {
		return status, data, err
	}
	if out != nil && len(bytes.TrimSpace(data)) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return status, data, Errorf(CodeProvider, "服务返回了无法解析的响应（HTTP %d）", status)
		}
	}
	return status, data, nil
}

// TokenResponse is the common OAuth 2.0 token endpoint response.
type TokenResponse struct {
	AccessToken      string `json:"access_token"`
	RefreshToken     string `json:"refresh_token"`
	TokenType        string `json:"token_type"`
	ExpiresIn        any    `json:"expires_in"`
	Scope            string `json:"scope"`
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
}

// BuildURL appends params to base as a query string.
func BuildURL(base string, params url.Values) string {
	if len(params) == 0 {
		return base
	}
	sep := "?"
	if strings.Contains(base, "?") {
		sep = "&"
	}
	return base + sep + params.Encode()
}

// CallbackParams are the OAuth parameters of one authorization response.
type CallbackParams struct {
	Code             string
	State            string
	Error            string
	ErrorDescription string
}

// ParseCallbackQuery reads the authorization response from a callback query.
func ParseCallbackQuery(q url.Values) CallbackParams {
	return CallbackParams{
		Code:             strings.TrimSpace(q.Get("code")),
		State:            strings.TrimSpace(q.Get("state")),
		Error:            strings.TrimSpace(q.Get("error")),
		ErrorDescription: strings.TrimSpace(q.Get("error_description")),
	}
}

// ParseCallbackURL reads the authorization response from a URL the user
// pasted (the address the provider redirected the browser to). Parameters are
// taken from the query, or from the fragment when the query carries neither a
// code nor an error.
func ParseCallbackURL(raw string) (CallbackParams, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return CallbackParams{}, Errorf(CodeInvalidInput, "请粘贴授权完成后浏览器地址栏中的完整地址")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return CallbackParams{}, Errorf(CodeInvalidInput, "粘贴的内容不是完整的网址")
	}
	p := ParseCallbackQuery(u.Query())
	if p.Code == "" && p.Error == "" && u.Fragment != "" {
		if fq, err := url.ParseQuery(u.Fragment); err == nil {
			p = ParseCallbackQuery(fq)
		}
	}
	if p.Code == "" && p.Error == "" {
		return CallbackParams{}, Errorf(CodeInvalidInput, "粘贴的地址中没有授权码（code）")
	}
	return p, nil
}

// ParsePastedToken reads a refresh token the user pasted: a bare token, a
// JSON object with "refresh_token", or a URL / string whose "#" fragment is
// base64(JSON) with "refresh_token" (the api.oplist.org callback format).
func ParsePastedToken(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", Errorf(CodeInvalidInput, "请粘贴刷新令牌")
	}
	var tok string
	switch {
	case strings.HasPrefix(s, "{"):
		var obj struct {
			RefreshToken string `json:"refresh_token"`
		}
		if err := json.Unmarshal([]byte(s), &obj); err != nil || obj.RefreshToken == "" {
			return "", Errorf(CodeInvalidInput, "粘贴的内容不是包含 refresh_token 的 JSON")
		}
		tok = strings.TrimSpace(obj.RefreshToken)
	case strings.Contains(s, "#"):
		// The OpenList token page keeps its result in the callback hash.
		frag := tokenFromFragment(s[strings.Index(s, "#")+1:])
		if frag == "" {
			return "", Errorf(CodeInvalidInput, "粘贴的地址中没有刷新令牌，请复制「刷新令牌（Refresh Token）」")
		}
		tok = strings.TrimSpace(frag)
	default:
		tok = s
	}
	if strings.ContainsAny(tok, " \t\r\n") {
		return "", Errorf(CodeInvalidInput, "刷新令牌中不能包含空白字符，请只粘贴令牌本身")
	}
	if n := len(tok); n < 16 || n > 8192 {
		return "", Errorf(CodeInvalidInput, "刷新令牌的长度无效，请粘贴完整的刷新令牌")
	}
	return tok, nil
}

// tokenFromFragment decodes the base64(JSON) callback hash of the OpenList
// token page; "" when the fragment is not that format.
func tokenFromFragment(frag string) string {
	data, err := base64.StdEncoding.DecodeString(frag)
	if err != nil {
		data, err = base64.RawStdEncoding.DecodeString(frag)
		if err != nil {
			return ""
		}
	}
	var obj struct {
		RefreshToken string `json:"refresh_token"`
	}
	if err := json.Unmarshal(data, &obj); err != nil {
		return ""
	}
	return obj.RefreshToken
}
