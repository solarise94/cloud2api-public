// Package goapiclient is the wire client of the GoAPI relay protocol: the
// remote cloud2api /v1 hierarchical file API, authenticated by an apikey
// header. It is shared by the GoAPI storage driver (drivers/goapi) and the
// goapi login adapter (pkg/loginsdk/adapters/goapi).
//
// Protocol bounds: http/https endpoints only (no query, fragment, userinfo
// or encoded traversal in the address); redirects are never followed
// (http.ErrUseLastResponse, so a key cannot leak to a redirect target); the
// apikey travels only in the `apikey` header of the configured endpoint and
// is scrubbed from every error; every call carries the relay hop counter
// from the context; metadata responses are strictly typed (missing fields,
// path mismatches and page inconsistencies are errors, never tolerated);
// upload success requires a fully-typed receipt verified against the
// streamed bytes.
package goapiclient

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc64"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	maxMetaBody     = 4 << 20
	metadataTimeout = 120 * time.Second
	listPerPage     = 1000
	maxListPages    = 20
	maxListEntries  = 10000
)

var (
	ErrInvalidPath     = errors.New("goapi: invalid path")
	ErrInvalidListing  = errors.New("goapi: peer listing violates the contract")
	ErrRedirected      = errors.New("goapi: peer responded with a redirect")
	ErrReceipt         = errors.New("goapi: upload receipt not confirmed")
	ErrBadPeerResponse = errors.New("goapi: peer response violates the contract")
)

var crc64Table = crc64.MakeTable(crc64.ECMA)

// CRCTable returns the protocol's CRC-64 table (CRC-64/XZ, ECMA) so callers
// hash streamed content with exactly the algorithm the receipt uses.
func CRCTable() *crc64.Table { return crc64Table }

// Error is a typed peer error. Code and Message are peer-controlled and are
// scrubbed of the apikey and bounded before leaving this package.
type Error struct {
	Status    int    `json:"-"`
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
	NotFound  bool   `json:"-"`
}

func (e *Error) Error() string {
	return fmt.Sprintf("goapi peer: status=%d code=%s: %s", e.Status, e.Code, e.Message)
}

type Client struct {
	base   string
	key    string
	meta   *http.Client
	stream *http.Client
}

// New validates the endpoint URL and returns a client. The key is kept for
// the `apikey` header only and scrubbed from every message.
func New(endpoint, key string) (*Client, error) {
	raw := strings.TrimRight(strings.TrimSpace(endpoint), "/")
	if raw == "" {
		return nil, errors.New("goapi: endpoint URL is required")
	}
	// Bare or encoded query/fragment/traversal forms are refused: the
	// endpoint is a origin-plus-prefix, never a URL with structure.
	if strings.ContainsAny(raw, "?#") {
		return nil, errors.New("goapi: endpoint URL must not contain a query or fragment")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, errors.New("goapi: endpoint URL cannot be parsed")
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, errors.New("goapi: endpoint URL must be an absolute http(s) address with a host")
	}
	if u.User != nil {
		return nil, errors.New("goapi: endpoint URL must not contain credentials")
	}
	if strings.Contains(u.RawPath, "%") {
		// RawPath is set exactly when the path carries percent-escapes; the
		// endpoint is a plain prefix and must not rely on decoding.
		return nil, errors.New("goapi: endpoint URL path must not contain percent-encoded characters")
	}
	for _, seg := range strings.Split(u.Path, "/") {
		if seg == ".." || seg == "." {
			return nil, errors.New("goapi: endpoint URL path must not traverse")
		}
	}
	if strings.TrimSpace(key) == "" {
		return nil, errors.New("goapi: API key is required")
	}
	transport := &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		DialContext:         (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout: 10 * time.Second,
		IdleConnTimeout:     90 * time.Second,
	}
	noRedirect := func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Client{
		base: raw,
		key:  strings.TrimSpace(key),
		meta: &http.Client{Timeout: metadataTimeout, Transport: transport, CheckRedirect: noRedirect},
		stream: &http.Client{
			Transport:     transport.Clone(),
			CheckRedirect: noRedirect,
		},
	}, nil
}

// redact removes the apikey from any text destined for errors or logs.
func (c *Client) redact(s string) string {
	if c.key == "" {
		return s
	}
	return strings.ReplaceAll(s, c.key, "***")
}

// peerText bounds, flattens and scrubs a peer-controlled string.
func (c *Client) peerText(s string) string {
	s = c.redact(strings.ReplaceAll(strings.TrimSpace(s), "\n", " "))
	if len(s) > 300 {
		s = s[:300] + "…"
	}
	return s
}

func (c *Client) do(ctx context.Context, streamCall bool, method, apiPath string, query url.Values, body io.Reader, contentLength int64, contentType string, extra ...http.Header) (*http.Response, error) {
	ref := c.base + apiPath
	if len(query) > 0 {
		ref += "?" + query.Encode()
	}
	hc := c.meta
	if streamCall {
		hc = c.stream
	}
	req, err := http.NewRequestWithContext(ctx, method, ref, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("apikey", c.key)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	for _, h := range extra {
		for k, vs := range h {
			for _, v := range vs {
				req.Header.Set(k, v)
			}
		}
	}
	if contentLength >= 0 {
		req.ContentLength = contentLength
	}
	if hop, ok := OutboundHop(ctx); ok {
		req.Header.Set(HopHeader, strconv.Itoa(hop))
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, &Error{Status: 0, Code: "network", Message: c.peerText(transportErrText(err))}
	}
	switch {
	case resp.StatusCode >= 300 && resp.StatusCode < 400:
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		_ = resp.Body.Close()
		return nil, &Error{Status: resp.StatusCode, Code: "redirect", Message: ErrRedirected.Error()}
	case resp.StatusCode < 200 || resp.StatusCode >= 300:
		return nil, c.statusError(resp)
	}
	return resp, nil
}

func transportErrText(err error) string {
	if t, ok := err.(interface{ Timeout() bool }); ok && t.Timeout() {
		return "upstream timeout"
	}
	return err.Error()
}

// statusError converts a non-2xx body into *Error via the peer's typed
// envelope, with code and message both scrubbed.
func (c *Client) statusError(resp *http.Response) error {
	defer func() { _ = resp.Body.Close() }()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, maxMetaBody))
	e := &Error{Status: resp.StatusCode, Code: fmt.Sprintf("http_%d", resp.StatusCode), Message: http.StatusText(resp.StatusCode)}
	var envelope struct {
		Error *struct {
			Code      string `json:"code"`
			Message   string `json:"message"`
			Retryable bool   `json:"retryable"`
		} `json:"error"`
	}
	if err := json.Unmarshal(data, &envelope); err == nil && envelope.Error != nil {
		e.Code = c.peerText(envelope.Error.Code)
		e.Message = c.peerText(envelope.Error.Message)
		e.Retryable = envelope.Error.Retryable
	}
	if e.Code == "path_not_found" {
		e.NotFound = true
	}
	return e
}

// decodeJSON reads a bounded body and unmarshals it.
func decodeJSON(resp *http.Response, out any) error {
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxMetaBody))
	if err != nil {
		return &Error{Status: resp.StatusCode, Code: "network", Message: "failed to read peer response"}
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("%w: peer response is not valid JSON", ErrBadPeerResponse)
	}
	return nil
}

// ---- wire types ----

// DriveView is one granted drive of the presenting key.
type DriveView struct {
	ID       string   `json:"id"`
	Provider string   `json:"provider"`
	Root     string   `json:"root"`
	Perms    []string `json:"perms"`
	Health   string   `json:"health"`
}

// Me is the subset of GET /v1/me the driver and adapter consume.
type Me struct {
	Key struct {
		ID           string `json:"id"`
		Prefix       string `json:"prefix"`
		DefaultDrive string `json:"default_drive"`
	} `json:"key"`
	Drives []DriveView `json:"drives"`
	// RelayHopLimit is the peer's advertised relay capability: the largest
	// X-GoAPI-Hop value its ingress accepts. Peers without it reset the
	// counter across the chain and are refused by ValidateRelayCapability.
	RelayHopLimit int `json:"relay_hop_limit"`
}

// Validate checks the grant list shape: non-empty, unique drive ids.
func (m *Me) Validate() error {
	seen := make(map[string]struct{}, len(m.Drives))
	for _, d := range m.Drives {
		if d.ID == "" {
			return fmt.Errorf("%w: peer granted a drive with an empty id", ErrBadPeerResponse)
		}
		if _, dup := seen[d.ID]; dup {
			return fmt.Errorf("%w: peer granted duplicate drive ids", ErrBadPeerResponse)
		}
		seen[d.ID] = struct{}{}
	}
	return nil
}

// ValidateRelayCapability requires the peer to advertise exactly this
// generation's hop bound; anything else means the peer resets hop counts
// and relay loops are not bounded across it.
func (m *Me) ValidateRelayCapability() error {
	if m.RelayHopLimit != MaxHops {
		return errors.New("需支持串联跳数保护的 cloud2api 上游版本（/v1/me relay_hop_limit=4）")
	}
	return nil
}

// HasDrive reports whether id is among the explicitly granted drives.
func (m *Me) HasDrive(id string) bool {
	for _, d := range m.Drives {
		if d.ID == id {
			return true
		}
	}
	return false
}

// GrantedIDs lists the granted drive ids (for error messages only).
func (m *Me) GrantedIDs() []string {
	out := make([]string, 0, len(m.Drives))
	for _, d := range m.Drives {
		out = append(out, d.ID)
	}
	return out
}

// Entry is one directory entry as the peer lists it.
type Entry struct {
	Name     string         `json:"name"`
	Path     string         `json:"path"`
	IsDir    bool           `json:"is_dir"`
	Size     int64          `json:"size"`
	Modified string         `json:"modified"`
	Hashes   map[string]any `json:"hashes"`
}

// ListResp is one page of a directory listing.
type ListResp struct {
	Items   []Entry `json:"items"`
	Total   int     `json:"total"`
	Page    int     `json:"page"`
	PerPage int     `json:"per_page"`
}

// Stat is the metadata of one path.
type Stat struct {
	Path     string         `json:"path"`
	Name     string         `json:"name"`
	IsDir    bool           `json:"is_dir"`
	Size     int64          `json:"size"`
	Modified string         `json:"modified"`
	Hashes   map[string]any `json:"hashes"`
}

// Receipt is the typed upload confirmation. Every field is a pointer: a
// receipt missing any of them (or with a zero size silently absent) is not
// a confirmation.
type Receipt struct {
	Path     *string `json:"path"`
	Size     *int64  `json:"size"`
	CRC64    *string `json:"crc64"`
	SHA256   *string `json:"sha256"`
	Conflict *string `json:"conflict"`
}

// DeleteResult is the per-path outcome of one delete call.
type DeleteResult struct {
	Path   *string `json:"path"`
	Status *string `json:"status"`
	Error  *string `json:"error"`
}

// ---- calls ----

// Me fetches and validates GET /v1/me.
func (c *Client) Me(ctx context.Context) (*Me, error) {
	resp, err := c.do(ctx, false, http.MethodGet, "/v1/me", nil, nil, 0, "")
	if err != nil {
		return nil, err
	}
	var me Me
	if err := decodeJSON(resp, &me); err != nil {
		return nil, err
	}
	if err := me.Validate(); err != nil {
		return nil, err
	}
	return &me, nil
}

// drivePath builds "/v1/drives/<drive>/<op>" after validating the drive id
// as a single clean segment; a hostile id is an error, never a panic.
func drivePath(drive, op string) (string, error) {
	if err := ValidDriveID(drive); err != nil {
		return "", err
	}
	return "/v1/drives/" + drive + "/" + op, nil
}

// ValidDriveID checks one drive id from the peer or the configuration.
func ValidDriveID(id string) error {
	if id == "" {
		return ErrInvalidPath
	}
	return checkSegment(id)
}

func (c *Client) List(ctx context.Context, drive, dir string, page int) (*ListResp, error) {
	dp, err := drivePath(drive, "list")
	if err != nil {
		return nil, err
	}
	q := url.Values{}
	q.Set("path", dir)
	q.Set("page", strconv.Itoa(page))
	q.Set("per_page", strconv.Itoa(listPerPage))
	resp, err := c.do(ctx, false, http.MethodGet, dp, q, nil, 0, "")
	if err != nil {
		return nil, err
	}
	var out ListResp
	if err := decodeJSON(resp, &out); err != nil {
		return nil, err
	}
	if out.Page != page || out.PerPage != listPerPage || out.Total < 0 {
		return nil, fmt.Errorf("%w: listing page metadata does not match the request", ErrBadPeerResponse)
	}
	return &out, nil
}

// ListAll fetches the complete listing of one directory with strict bounds:
// the total is refused upfront when above the cap, every page's metadata and
// every entry are validated, entries are unique, the collected count must
// equal the declared total exactly (no premature end, no overcount) and the
// context is honoured between pages.
func (c *Client) ListAll(ctx context.Context, drive, dir string) ([]Entry, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	first, err := c.List(ctx, drive, dir, 1)
	if err != nil {
		return nil, err
	}
	if first.Total > maxListEntries {
		return nil, fmt.Errorf("%w: directory exceeds the relay listing limit", ErrBadPeerResponse)
	}
	out := make([]Entry, 0, min(first.Total, maxListEntries))
	if err := c.collectPage(ctx, drive, dir, first, &out); err != nil {
		return nil, err
	}
	for page := 2; len(out) < first.Total; page++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if page > maxListPages {
			return nil, fmt.Errorf("%w: directory exceeds the relay listing limit", ErrBadPeerResponse)
		}
		resp, err := c.List(ctx, drive, dir, page)
		if err != nil {
			return nil, err
		}
		if resp.Total != first.Total {
			return nil, fmt.Errorf("%w: peer listing total changed mid-pagination", ErrBadPeerResponse)
		}
		if err := c.collectPage(ctx, drive, dir, resp, &out); err != nil {
			return nil, err
		}
	}
	if len(out) != first.Total {
		return nil, fmt.Errorf("%w: peer listing ended before the declared total", ErrBadPeerResponse)
	}
	return out, nil
}

func (c *Client) collectPage(ctx context.Context, drive, dir string, resp *ListResp, out *[]Entry) error {
	if len(resp.Items) == 0 && len(*out) < resp.Total {
		return fmt.Errorf("%w: peer listing ended before the declared total", ErrBadPeerResponse)
	}
	seen := map[string]struct{}{}
	for _, e := range resp.Items {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := CheckEntry(dir, e.Name, e.Path, e.IsDir, e.Size); err != nil {
			return fmt.Errorf("%w: entry outside the contract", ErrInvalidListing)
		}
		if _, dup := seen[e.Path]; dup {
			return fmt.Errorf("%w: duplicate entry", ErrInvalidListing)
		}
		seen[e.Path] = struct{}{}
		*out = append(*out, e)
		if len(*out) > resp.Total {
			return fmt.Errorf("%w: peer sent more entries than declared", ErrBadPeerResponse)
		}
	}
	return nil
}

// Stat fetches GET /v1/drives/{drive}/stat. The peer must echo the exact
// requested path; anything else is a contract violation.
func (c *Client) Stat(ctx context.Context, drive, p string) (*Stat, error) {
	dp, err := drivePath(drive, "stat")
	if err != nil {
		return nil, err
	}
	q := url.Values{}
	q.Set("path", p)
	resp, err := c.do(ctx, false, http.MethodGet, dp, q, nil, 0, "")
	if err != nil {
		return nil, err
	}
	var out Stat
	if err := decodeJSON(resp, &out); err != nil {
		return nil, err
	}
	if out.Path != p {
		return nil, fmt.Errorf("%w: peer stat answered for %q", ErrBadPeerResponse, c.redact(out.Path))
	}
	return &out, nil
}

// Mkdir creates a directory; an already-existing directory is success. The
// response must be typed ({path, created}) and echo the requested path.
func (c *Client) Mkdir(ctx context.Context, drive, p string) error {
	dp, err := drivePath(drive, "mkdir")
	if err != nil {
		return err
	}
	var out struct {
		Path    *string `json:"path"`
		Created *bool   `json:"created"`
	}
	if err := c.post(ctx, dp, map[string]string{"path": p}, &out); err != nil {
		return err
	}
	if out.Path == nil || out.Created == nil {
		return fmt.Errorf("%w: mkdir response is not typed", ErrBadPeerResponse)
	}
	if *out.Path != p {
		return fmt.Errorf("%w: mkdir answered for %q", ErrBadPeerResponse, c.redact(*out.Path))
	}
	return nil
}

// Move renames/moves one path (the peer's move covers both); the response
// must echo the exact from/to.
func (c *Client) Move(ctx context.Context, drive, from, to string) error {
	dp, err := drivePath(drive, "move")
	if err != nil {
		return err
	}
	var out struct {
		From *string `json:"from"`
		To   *string `json:"to"`
	}
	if err := c.post(ctx, dp, map[string]string{"from": from, "to": to, "on_conflict": "fail"}, &out); err != nil {
		return err
	}
	if out.From == nil || out.To == nil || *out.From != from || *out.To != to {
		return fmt.Errorf("%w: move response does not match the request", ErrBadPeerResponse)
	}
	return nil
}

// Remove deletes one path and checks its typed per-path result; the result
// must address exactly the requested path.
func (c *Client) Remove(ctx context.Context, drive, p string) error {
	dp, err := drivePath(drive, "delete")
	if err != nil {
		return err
	}
	var out struct {
		Results []DeleteResult `json:"results"`
	}
	if err := c.post(ctx, dp, map[string]any{"paths": []string{p}}, &out); err != nil {
		return err
	}
	if len(out.Results) != 1 || out.Results[0].Path == nil || *out.Results[0].Path != p {
		return fmt.Errorf("%w: delete result does not match the request", ErrBadPeerResponse)
	}
	switch deref(out.Results[0].Status) {
	case "deleted", "trashed", "already_absent":
		return nil
	case "":
		return fmt.Errorf("%w: delete result is not typed", ErrBadPeerResponse)
	default:
		return &Error{Status: 200, Code: "delete_failed", Message: c.peerText(deref(out.Results[0].Error))}
	}
}

func (c *Client) post(ctx context.Context, apiPath string, payload, out any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	resp, err := c.do(ctx, false, http.MethodPost, apiPath, nil, bytes.NewReader(data), int64(len(data)), "application/json")
	if err != nil {
		return err
	}
	return decodeJSON(resp, out)
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// Download opens GET /v1/drives/{drive}/files for one path. Range requests
// must be honored with 206 and a Content-Range matching the request; a peer
// that answers a ranged read with 200 (or the wrong window) fails closed
// instead of returning bytes from the wrong offset. Full reads must be 200
// and never longer than the object. Bounded reads return a body that errors
// when the peer delivers fewer bytes than the window. Only rangeHdr is
// forwarded — never inbound credentials.
func (c *Client) Download(ctx context.Context, drive, p, rangeHdr string) (*http.Response, error) {
	dp, err := drivePath(drive, "files")
	if err != nil {
		return nil, err
	}
	q := url.Values{}
	q.Set("path", p)
	hdr := http.Header{}
	if rangeHdr != "" {
		hdr.Set("Range", rangeHdr)
	}
	resp, err := c.do(ctx, true, http.MethodGet, dp, q, nil, 0, "", hdr)
	if err != nil {
		return nil, err
	}
	if rangeHdr != "" {
		if resp.StatusCode != http.StatusPartialContent {
			drainClose(resp)
			return nil, fmt.Errorf("%w: peer answered a ranged read with HTTP %d", ErrBadPeerResponse, resp.StatusCode)
		}
		window, err := checkContentRange(resp.Header.Get("Content-Range"), rangeHdr)
		if err != nil {
			drainClose(resp)
			return nil, err
		}
		if window > 0 {
			// The consumer expects exactly this many bytes: a short body is
			// a read error, extra bytes are refused.
			resp.Body = &exactLenReader{rc: resp.Body, remaining: window}
		}
		return resp, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, c.statusError(resp)
	}
	return resp, nil
}

func drainClose(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	_ = resp.Body.Close()
}

// checkContentRange validates "bytes=<start>-<end>[/<total>]" against the
// request header and returns the window size (0 for open-ended reads).
func checkContentRange(contentRange, requestRange string) (int64, error) {
	lower := strings.ReplaceAll(strings.ToLower(contentRange), " ", "")
	cr := strings.TrimPrefix(lower, "bytes")
	cr = strings.TrimPrefix(cr, "=")
	if lower == cr || cr == "" {
		return 0, fmt.Errorf("%w: peer 206 lacks a bytes Content-Range", ErrBadPeerResponse)
	}
	spec := cr
	if i := strings.IndexByte(cr, '/'); i >= 0 {
		spec = cr[:i]
	}
	dash := strings.IndexByte(spec, '-')
	if dash <= 0 {
		return 0, fmt.Errorf("%w: peer Content-Range is not a range", ErrBadPeerResponse)
	}
	gotStart, err1 := strconv.ParseUint(spec[:dash], 10, 64)
	gotEnd, err2 := strconv.ParseUint(spec[dash+1:], 10, 64)
	if err1 != nil || err2 != nil || gotEnd < gotStart {
		return 0, fmt.Errorf("%w: peer Content-Range is not parseable", ErrBadPeerResponse)
	}
	req := strings.TrimPrefix(strings.ToLower(requestRange), "bytes=")
	if strings.HasSuffix(req, "-") {
		wantStart, err := strconv.ParseUint(req[:len(req)-1], 10, 64)
		if err != nil || gotStart != wantStart {
			return 0, fmt.Errorf("%w: peer 206 starts at the wrong offset", ErrBadPeerResponse)
		}
		if gotEnd-gotStart >= uint64(1<<63-1) {
			return 0, fmt.Errorf("%w: peer range window is too large", ErrBadPeerResponse)
		}
		return int64(gotEnd-gotStart) + 1, nil
	}
	dash = strings.IndexByte(req, '-')
	if dash <= 0 {
		return 0, fmt.Errorf("%w: unsupported range request", ErrBadPeerResponse)
	}
	wantStart, err1 := strconv.ParseUint(req[:dash], 10, 64)
	wantEnd, err2 := strconv.ParseUint(req[dash+1:], 10, 64)
	if err1 != nil || err2 != nil || wantEnd < wantStart {
		return 0, fmt.Errorf("%w: unsupported range request", ErrBadPeerResponse)
	}
	if gotStart != wantStart || gotEnd != wantEnd {
		return 0, fmt.Errorf("%w: peer 206 covers the wrong window", ErrBadPeerResponse)
	}
	if gotEnd-gotStart >= uint64(1<<63-1) {
		return 0, fmt.Errorf("%w: peer range window is too large", ErrBadPeerResponse)
	}
	return int64(gotEnd-gotStart) + 1, nil
}

// exactLenReader errors when the body ends before the promised window is
// delivered, or when the peer sends even one byte beyond it.
type exactLenReader struct {
	rc        io.ReadCloser
	remaining int64
}

func (e *exactLenReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if e.remaining <= 0 {
		var probe [1]byte
		n, err := e.rc.Read(probe[:])
		if n > 0 {
			return 0, fmt.Errorf("%w: peer sent more bytes than the 206 window", ErrBadPeerResponse)
		}
		return 0, err
	}
	if len(p) > int(e.remaining) {
		p = p[:e.remaining]
	}
	n, err := e.rc.Read(p)
	e.remaining -= int64(n)
	if err == io.EOF && e.remaining > 0 {
		return n, fmt.Errorf("%w: peer 206 body ended %d bytes early", ErrBadPeerResponse, e.remaining)
	}
	return n, err
}

func (e *exactLenReader) Close() error { return e.rc.Close() }

// Upload performs PUT /v1/drives/{drive}/files and returns the typed
// receipt. size > 0 uses the raw-body form with an exact Content-Length;
// size == 0 (and any unknown size) uses the multipart `file` form, since
// the peer requires a positive length for raw bodies. The request is sent
// exactly once — never retried. Success requires HTTP 200 plus a fully
// typed receipt; 202 (a queued upload), a missing field or a request body
// the peer did not consume all leave the outcome unconfirmed.
func (c *Client) Upload(ctx context.Context, drive, p, name, onConflict string, size int64, body io.Reader) (*Receipt, error) {
	dp, err := drivePath(drive, "files")
	if err != nil {
		return nil, err
	}
	q := url.Values{}
	q.Set("path", p)
	q.Set("on_conflict", onConflict)
	if size > 0 {
		resp, err := c.do(ctx, true, http.MethodPut, dp, q, body, size, "application/octet-stream", nil)
		if err != nil {
			return nil, err
		}
		return c.readReceipt(resp)
	}
	// Multipart form streamed through a pipe; the writer goroutine is
	// always joined before a receipt is accepted, so a peer that responds
	// without consuming the body cannot pass as a confirmation.
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	bodyDone := make(chan error, 1)
	go func() {
		part, perr := mw.CreateFormFile("file", name)
		var cerr error
		if perr == nil {
			_, cerr = io.Copy(part, body)
		}
		if perr == nil && cerr == nil {
			cerr = mw.Close()
		}
		_ = pw.CloseWithError(errors.Join(perr, cerr))
		bodyDone <- errors.Join(perr, cerr)
	}()
	resp, err := c.do(ctx, true, http.MethodPut, dp, q, pr, -1, mw.FormDataContentType(), nil)
	if err != nil {
		_ = pr.CloseWithError(err)
		<-bodyDone
		return nil, err
	}
	receipt, rerr := c.readReceipt(resp)
	if berr := <-bodyDone; berr != nil && rerr == nil {
		return nil, fmt.Errorf("%w: peer did not consume the request body", ErrReceipt)
	}
	return receipt, rerr
}

// readReceipt strictly parses the typed upload confirmation: HTTP 200 only
// (a 202 is a queued upload, never a confirmation) and every field present.
func (c *Client) readReceipt(resp *http.Response) (*Receipt, error) {
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxMetaBody))
	if err != nil {
		return nil, fmt.Errorf("%w: failed to read the upload response", ErrReceipt)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: peer answered HTTP %d", ErrReceipt, resp.StatusCode)
	}
	var r Receipt
	if jerr := json.Unmarshal(data, &r); jerr != nil {
		return nil, fmt.Errorf("%w: response is not a typed receipt", ErrReceipt)
	}
	if r.Path == nil || r.Size == nil || r.CRC64 == nil || r.SHA256 == nil || r.Conflict == nil {
		return nil, fmt.Errorf("%w: receipt is missing typed fields", ErrReceipt)
	}
	switch *r.Conflict {
	case "created", "overwritten", "renamed":
	default:
		return nil, fmt.Errorf("%w: receipt conflict value is outside the contract", ErrReceipt)
	}
	r.Path = ptr(c.redact(*r.Path))
	return &r, nil
}

func ptr[T any](v T) *T { return &v }

// VerifyReceipt compares the receipt against the content that was streamed:
// the intended remote path, the byte count and the CRC-64/XZ and SHA-256 of
// the uploaded bytes. Error texts stay generic — receipt values are
// peer-controlled and never echoed.
func VerifyReceipt(r *Receipt, wantPath string, size int64, crc uint64, sha []byte) error {
	if r.Path == nil || *r.Path != wantPath {
		return fmt.Errorf("%w: receipt path does not match the requested path", ErrReceipt)
	}
	if r.Size == nil || *r.Size != size {
		return fmt.Errorf("%w: receipt size does not match the streamed content", ErrReceipt)
	}
	if r.CRC64 == nil || *r.CRC64 != strconv.FormatUint(crc, 10) {
		return fmt.Errorf("%w: receipt crc64 does not match the streamed content", ErrReceipt)
	}
	if r.SHA256 == nil || !strings.EqualFold(*r.SHA256, hex.EncodeToString(sha)) {
		return fmt.Errorf("%w: receipt sha256 does not match the streamed content", ErrReceipt)
	}
	return nil
}
