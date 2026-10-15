package agentapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"cloud2api/internal/store"
)

// ---- removed approval endpoints (regression) ----

func TestRequestRoutesRemoved(t *testing.T) {
	h := newHarness(t)
	cases := []struct {
		method, target string
	}{
		{http.MethodGet, "/v1/requests"},
		{http.MethodPost, "/v1/requests"},
		{http.MethodGet, "/v1/requests/1"},
	}
	for _, tc := range cases {
		rec, doc := h.do(tc.method, tc.target, chrootPlain, []byte(`{}`), jsonCT())
		if rec.Code != http.StatusNotFound || errCode(doc) != CodeNotFound {
			t.Fatalf("%s %s = %d %v, want 404 not_found", tc.method, tc.target, rec.Code, doc)
		}
	}
}

// ---- durable upload receipts ----

// receiptStatusOf queries GET /api/files/status/{id} for one key.
func receiptStatusOf(t *testing.T, h *harness, key, id string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	rec, doc := h.do(http.MethodGet, "/api/files/status/"+id, key, nil, nil)
	return rec, doc
}

func TestGokapiUploadReceiptHappyPath(t *testing.T) {
	h := newHarness(t)
	h.addRelayKey(t, "relay", relayPlain, store.RelaySettings{})

	content := []byte("123456789")
	rec, doc := gokapiUpload(t, h, relayPlain, map[string]string{"allowedDownloads": "0", "expiryDays": "7"}, "check.bin", content, "application/octet-stream")
	if rec.Code != 200 || doc["Result"] != "OK" {
		t.Fatalf("add = %d %v", rec.Code, doc)
	}
	// Additive receipt fields and header, existing fields untouched.
	id, _ := doc["UploadId"].(string)
	if id == "" || doc["Status"] != store.RelayUploadCompleted {
		t.Fatalf("receipt fields = %v (id=%q)", doc, id)
	}
	if got := rec.Header().Get("X-Upload-ID"); got != id {
		t.Fatalf("X-Upload-ID = %q, want %q", got, id)
	}
	fi := fileInfoOf(t, doc)
	if fi["CRC64"] != "11051210869376104954" {
		t.Fatalf("CRC64 = %v", fi["CRC64"])
	}

	// The status endpoint answers the same key with the stored proof.
	srec, sdoc := receiptStatusOf(t, h, relayPlain, id)
	if srec.Code != 200 || sdoc["Status"] != store.RelayUploadCompleted || sdoc["UploadId"] != id {
		t.Fatalf("status = %d %v", srec.Code, sdoc)
	}
	sfi := fileInfoOf(t, sdoc)
	if sfi["Id"] != fi["Id"] || sfi["CRC64"] != "11051210869376104954" {
		t.Fatalf("status FileInfo = %v", sfi)
	}

	// A second key sees nothing; no key sees nothing; the id never leaks
	// across keys.
	other := "sk-c2a-re1ay002-relaysecretrelaysecre2"
	h.addRelayKey(t, "relay2", other, store.RelaySettings{})
	if rec, _ := receiptStatusOf(t, h, other, id); rec.Code != http.StatusNotFound {
		t.Fatalf("cross-key status = %d, want 404", rec.Code)
	}
	if rec, _ := receiptStatusOf(t, h, relayPlain, "no-such-upload-id"); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown status = %d, want 404", rec.Code)
	}
	rec, _ = h.do(http.MethodGet, "/api/files/status/"+id, "", nil, nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d, want 401", rec.Code)
	}

	// Client-chosen ids are honored — as the immutable identity of ONE
	// attempt: another key is refused, and even the SAME key cannot reuse the
	// id for a second attempt; a retry needs a fresh id and leaves the old
	// receipt untouched.
	rec, doc = gokapiUploadRaw(t, h, relayPlain, map[string]string{"allowedDownloads": "0"}, "x.bin", []byte("x"), "", map[string]string{"X-Upload-ID": "client-id-0001"})
	if rec.Code != 200 || doc["UploadId"] != "client-id-0001" || doc["Status"] != store.RelayUploadCompleted {
		t.Fatalf("client id = %d %v", rec.Code, doc)
	}
	rec, doc = gokapiUploadRaw(t, h, other, map[string]string{"allowedDownloads": "0"}, "x.bin", []byte("x"), "", map[string]string{"X-Upload-ID": "client-id-0001"})
	if rec.Code != http.StatusConflict {
		t.Fatalf("cross-key id reuse = %d %v, want 409", rec.Code, doc)
	}
	rec, doc = gokapiUploadRaw(t, h, relayPlain, map[string]string{"allowedDownloads": "0"}, "x.bin", []byte("xx"), "", map[string]string{"X-Upload-ID": "client-id-0001"})
	if rec.Code != http.StatusConflict || doc["Status"] == store.RelayUploadCompleted {
		t.Fatalf("same-key id reuse = %d %v, want 409 and no completion claim", rec.Code, doc)
	}
	// The completed outcome survived the refused reuse untouched.
	srec, sdoc = receiptStatusOf(t, h, relayPlain, "client-id-0001")
	if srec.Code != 200 || sdoc["Status"] != store.RelayUploadCompleted {
		t.Fatalf("completed receipt mutated by refused reuse: %d %v", srec.Code, sdoc)
	}
	// A retry with a FRESH id works normally.
	rec, doc = gokapiUploadRaw(t, h, relayPlain, map[string]string{"allowedDownloads": "0"}, "x.bin", []byte("xx"), "", map[string]string{"X-Upload-ID": "client-id-0002"})
	if rec.Code != 200 || doc["Status"] != store.RelayUploadCompleted {
		t.Fatalf("fresh-id retry = %d %v", rec.Code, doc)
	}
	// Invalid client ids are rejected before any work.
	rec, doc = gokapiUploadRaw(t, h, relayPlain, map[string]string{"allowedDownloads": "0"}, "x.bin", []byte("x"), "", map[string]string{"X-Upload-ID": "bad id!"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid id = %d %v", rec.Code, doc)
	}
}

// TestGokapiUploadIDConcurrentSameKey pins the atomic reservation: of many
// concurrent uploads presenting the SAME id, exactly one runs; every other
// gets a 409 and the receipt keeps the single truthful outcome.
func TestGokapiUploadIDConcurrentSameKey(t *testing.T) {
	h := newHarness(t)
	h.addRelayKey(t, "relay", relayPlain, store.RelaySettings{})

	const id = "race-id-000001"
	const n = 8
	var wg sync.WaitGroup
	type result struct {
		code   int
		status any
	}
	results := make(chan result, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec, doc := gokapiUploadRaw(t, h, relayPlain, map[string]string{"allowedDownloads": "0"}, fmt.Sprintf("r%d.bin", i), []byte("race"), "", map[string]string{"X-Upload-ID": id})
			results <- result{rec.Code, doc["Status"]}
		}()
	}
	wg.Wait()
	close(results)
	okCount, conflictCount := 0, 0
	for r := range results {
		switch r.code {
		case 200:
			okCount++
			if r.status != store.RelayUploadCompleted {
				t.Fatalf("winner status = %v", r.status)
			}
		case http.StatusConflict:
			conflictCount++
		default:
			t.Fatalf("unexpected concurrent result %d", r.code)
		}
	}
	if okCount != 1 || conflictCount != n-1 {
		t.Fatalf("wins=%d conflicts=%d, want 1/%d", okCount, conflictCount, n-1)
	}
	// The losers never reached the storage backend either.
	h.fake.mu.Lock()
	puts := len(h.fake.puts)
	h.fake.mu.Unlock()
	if puts != 1 {
		t.Fatalf("losing reservations reached upstream: %d puts, want exactly 1", puts)
	}
	srec, sdoc := receiptStatusOf(t, h, relayPlain, id)
	if srec.Code != 200 || sdoc["Status"] != store.RelayUploadCompleted {
		t.Fatalf("receipt after race = %d %v", srec.Code, sdoc)
	}
}

// gokapiUploadRaw uploads via /api/files/add with extra request headers.
func gokapiUploadRaw(t *testing.T, h *harness, key string, fields map[string]string, filename string, content []byte, contentType string, hdrs map[string]string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	ct, body := multipartBody(t, fields, filename, content, contentType)
	req := httptest.NewRequest(http.MethodPost, "/api/files/add", bytes.NewReader(body))
	req.Header.Set("apikey", key)
	req.Header.Set("Content-Type", ct)
	for k, v := range hdrs {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	var doc map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &doc)
	return rec, doc
}

// TestGokapiUploadReceiptInterruptedAtBodyParse proves the receipt exists (and
// ends up interrupted, never completed) when the client disconnects before the
// request body could even be parsed — upstream is never called.
func TestGokapiUploadReceiptInterruptedAtBodyParse(t *testing.T) {
	h := newHarness(t)
	h.addRelayKey(t, "relay", relayPlain, store.RelaySettings{})

	req := httptest.NewRequest(http.MethodPost, "/api/files/add", strings.NewReader("not-a-multipart-body"))
	req.Header.Set("apikey", relayPlain)
	req.Header.Set("Content-Type", "multipart/form-data; boundary=x")
	req.Header.Set("Content-Length", "4096")
	ctx, cancel := context.WithCancel(req.Context())
	req = req.WithContext(ctx)
	// The client is gone before the handler runs.
	cancel()
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)

	var doc map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &doc)
	id, _ := doc["UploadId"].(string)
	if id == "" {
		t.Fatalf("no receipt in error response: %d %v", rec.Code, doc)
	}
	if doc["Status"] != store.RelayUploadInterrupted {
		t.Fatalf("status = %v, want interrupted", doc["Status"])
	}
	if msg, _ := doc["Message"].(string); !strings.Contains(msg, "partial or complete object") {
		t.Fatalf("message not honest about remote state: %q", msg)
	}
	// The receipt is queryable after reconnecting and stays interrupted.
	srec, sdoc := receiptStatusOf(t, h, relayPlain, id)
	if srec.Code != 200 || sdoc["Status"] != store.RelayUploadInterrupted {
		t.Fatalf("status after reconnect = %d %v", srec.Code, sdoc)
	}
	// No upstream put happened.
	h.fake.mu.Lock()
	puts := len(h.fake.puts)
	h.fake.mu.Unlock()
	if puts != 0 {
		t.Fatalf("interrupted upload reached upstream (%d puts)", puts)
	}
}

func TestGokapiUploadReceiptFailedOnUpstreamError(t *testing.T) {
	h := newHarness(t)
	h.addRelayKey(t, "relay", relayPlain, store.RelaySettings{})
	h.fake.mu.Lock()
	h.fake.putReject = true
	h.fake.mu.Unlock()

	rec, doc := gokapiUpload(t, h, relayPlain, map[string]string{"allowedDownloads": "0"}, "f.bin", []byte("data"), "")
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("add = %d %v", rec.Code, doc)
	}
	id, _ := doc["UploadId"].(string)
	if id == "" || doc["Status"] != store.RelayUploadFailed {
		t.Fatalf("receipt = %v", doc)
	}
	srec, sdoc := receiptStatusOf(t, h, relayPlain, id)
	if srec.Code != 200 || sdoc["Status"] != store.RelayUploadFailed {
		t.Fatalf("status = %d %v", srec.Code, sdoc)
	}
}

// TestGokapiUploadEnvelopeValidation pins the authoritative-completion rule:
// HTTP 200 without a valid success envelope (present success code) is NEVER a
// completion — neither in the Gokapi layer nor on /v1.
func TestGokapiUploadEnvelopeValidation(t *testing.T) {
	h := newHarness(t)
	h.addRelayKey(t, "relay", relayPlain, store.RelaySettings{})
	h.fake.mu.Lock()
	h.fake.putGarbage = true
	h.fake.mu.Unlock()

	rec, doc := gokapiUpload(t, h, relayPlain, map[string]string{"allowedDownloads": "0"}, "g.bin", []byte("data"), "")
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("add = %d %v", rec.Code, doc)
	}
	id, _ := doc["UploadId"].(string)
	if id == "" || doc["Status"] != store.RelayUploadFailed {
		t.Fatalf("receipt = %v", doc)
	}
	if msg, _ := doc["Message"].(string); !strings.Contains(msg, "not a valid success envelope") {
		t.Fatalf("message = %q", msg)
	}
	srec, sdoc := receiptStatusOf(t, h, relayPlain, id)
	if srec.Code != 200 || sdoc["Status"] != store.RelayUploadFailed {
		t.Fatalf("status = %d %v", srec.Code, sdoc)
	}

	// The /v1 data plane enforces the same rule.
	rec, doc = h.do(http.MethodPut, "/v1/files?path=/g.bin", chrootPlain, []byte("data"),
		map[string]string{"Content-Type": "application/octet-stream"})
	if rec.Code != http.StatusBadGateway || errCode(doc) != CodeUpstreamError {
		t.Fatalf("/v1 add = %d %v", rec.Code, doc)
	}
	if msg := fmt.Sprint(doc["error"].(map[string]any)["message"]); !strings.Contains(msg, "not a valid success envelope") {
		t.Fatalf("/v1 message = %q", msg)
	}
}

// TestGokapiUploadReceiptQuotaFailure proves the quota rejection is a failed
// (not interrupted) receipt with the reason.
func TestGokapiUploadReceiptQuotaFailure(t *testing.T) {
	h := newHarness(t)
	h.addRelayKey(t, "relay", relayPlain, store.RelaySettings{QuotaBytes: 4})
	rec, doc := gokapiUpload(t, h, relayPlain, map[string]string{"allowedDownloads": "0"}, "big.bin", []byte("way-more-than-four"), "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("add = %d %v", rec.Code, doc)
	}
	id, _ := doc["UploadId"].(string)
	srec, sdoc := receiptStatusOf(t, h, relayPlain, id)
	if srec.Code != 200 || sdoc["Status"] != store.RelayUploadFailed {
		t.Fatalf("status = %d %v", srec.Code, sdoc)
	}
	if msg, _ := sdoc["Message"].(string); !strings.Contains(msg, "quota") {
		t.Fatalf("message = %q", msg)
	}
}

// TestGokapiChunkCompleteReceipt covers the chunk flow: staged acks are not
// completion (and may repeat one id), blocking completion is, duplicates of a
// finished attempt are refused, nonblocking acks stay uploading until the
// background attempt lands a queryable terminal state.
func TestGokapiChunkCompleteReceipt(t *testing.T) {
	h := newHarness(t)
	h.addRelayKey(t, "relay", relayPlain, store.RelaySettings{})
	const uploadID = "chunked-up-0001"

	stage := func(id, uuid string) {
		t.Helper()
		ct, body := multipartBody(t, map[string]string{"uuid": uuid, "filesize": "11", "offset": "0"}, "part.bin", []byte("hello chunk"), "")
		req := httptest.NewRequest(http.MethodPost, "/api/chunk/add", bytes.NewReader(body))
		req.Header.Set("apikey", relayPlain)
		req.Header.Set("Content-Type", ct)
		req.Header.Set("X-Upload-ID", id)
		rec := httptest.NewRecorder()
		h.handler.ServeHTTP(rec, req)
		var d map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &d)
		if rec.Code != 200 || d["Result"] != "OK" || d["UploadId"] != id || d["Status"] != store.RelayUploadUploading {
			t.Fatalf("chunk add = %d %v", rec.Code, d)
		}
	}
	complete := func(uuid string, nonblocking bool) (*httptest.ResponseRecorder, map[string]any) {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/api/chunk/complete", nil)
		req.Header.Set("apikey", relayPlain)
		req.Header.Set("uuid", uuid)
		req.Header.Set("filename", "assembled.bin")
		req.Header.Set("filesize", "11")
		if nonblocking {
			req.Header.Set("nonblocking", "true")
		}
		rec := httptest.NewRecorder()
		h.handler.ServeHTTP(rec, req)
		var d map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &d)
		return rec, d
	}

	// Staging may repeat the same id: metadata only, no state change.
	stage(uploadID, "chunkuuid-01")
	stage(uploadID, "chunkuuid-01")

	// Blocking completion is authoritative.
	rec, doc := complete("chunkuuid-01", false)
	if rec.Code != 200 || doc["Status"] != store.RelayUploadCompleted || doc["UploadId"] != uploadID {
		t.Fatalf("blocking complete = %d %v", rec.Code, doc)
	}

	// A duplicate completion of the finished attempt is refused and cannot
	// overwrite the truthful outcome.
	rec, doc = complete("chunkuuid-01", false)
	if rec.Code != http.StatusConflict || doc["Status"] == store.RelayUploadCompleted {
		t.Fatalf("duplicate complete = %d %v, want 409 without a completion claim", rec.Code, doc)
	}
	srec, sdoc := receiptStatusOf(t, h, relayPlain, uploadID)
	if srec.Code != 200 || sdoc["Status"] != store.RelayUploadCompleted {
		t.Fatalf("receipt after duplicate complete = %d %v", srec.Code, sdoc)
	}

	// Nonblocking completion: ack stays uploading, the background terminal
	// state is pollable — including failures.
	stage("chunked-up-0002", "chunkuuid-02")
	rec, doc = complete("chunkuuid-02", true)
	if rec.Code != 200 || doc["Status"] != store.RelayUploadUploading {
		t.Fatalf("nonblocking ack = %d %v", rec.Code, doc)
	}
	id2, _ := doc["UploadId"].(string)
	deadline := time.Now().Add(10 * time.Second)
	for {
		srec, sdoc := receiptStatusOf(t, h, relayPlain, id2)
		if srec.Code == 200 && sdoc["Status"] != store.RelayUploadUploading {
			if sdoc["Status"] != store.RelayUploadCompleted {
				t.Fatalf("background state = %v %v", sdoc["Status"], sdoc["Message"])
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("nonblocking upload never completed: %d %v", srec.Code, sdoc)
		}
		time.Sleep(50 * time.Millisecond)
	}

	// A nonblocking background failure is queryable, not log-only.
	h.fake.mu.Lock()
	h.fake.putReject = true
	h.fake.mu.Unlock()
	stage("chunked-up-0003", "chunkuuid-03")
	rec, doc = complete("chunkuuid-03", true)
	if rec.Code != 200 {
		t.Fatalf("nonblocking ack (doomed) = %d %v", rec.Code, doc)
	}
	id3, _ := doc["UploadId"].(string)
	deadline = time.Now().Add(10 * time.Second)
	for {
		srec, sdoc := receiptStatusOf(t, h, relayPlain, id3)
		if srec.Code == 200 && sdoc["Status"] == store.RelayUploadFailed {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("background failure never became queryable: %d %v", srec.Code, sdoc)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestGokapiConcurrentChunkComplete pins duplicate-worker protection: of two
// completions of the same attempt, the atomic claim lets exactly ONE reach
// the upstream (asserted on the fake's put log); the loser answers 409 with
// no receipt write, and the receipt keeps one truthful terminal outcome.
func TestGokapiConcurrentChunkComplete(t *testing.T) {
	h := newHarness(t)
	h.addRelayKey(t, "relay", relayPlain, store.RelaySettings{})
	putStarted := make(chan struct{}, 1)
	putRelease := make(chan struct{})
	release := sync.OnceFunc(func() { close(putRelease) })
	defer release()
	h.fake.mu.Lock()
	h.fake.putStarted = putStarted
	h.fake.putRelease = putRelease
	h.fake.mu.Unlock()

	ct, body := multipartBody(t, map[string]string{"uuid": "chunkuuid-r1", "filesize": "11", "offset": "0"}, "part.bin", []byte("hello chunk"), "")
	req := httptest.NewRequest(http.MethodPost, "/api/chunk/add", bytes.NewReader(body))
	req.Header.Set("apikey", relayPlain)
	req.Header.Set("Content-Type", ct)
	req.Header.Set("X-Upload-ID", "chunked-up-race")
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("chunk add = %d %s", rec.Code, rec.Body.String())
	}

	mk := func(nonblocking bool) *http.Request {
		req := httptest.NewRequest(http.MethodPost, "/api/chunk/complete", nil)
		req.Header.Set("apikey", relayPlain)
		req.Header.Set("uuid", "chunkuuid-r1")
		req.Header.Set("filename", "race.bin")
		req.Header.Set("filesize", "11")
		if nonblocking {
			req.Header.Set("nonblocking", "true")
		}
		return req
	}
	// The winner claims the completion atomically; the duplicate — sent while
	// the winner's background upload is still running — loses the claim
	// BEFORE any upstream work.
	r1 := httptest.NewRecorder()
	h.handler.ServeHTTP(r1, mk(true))
	if r1.Code != 200 {
		t.Fatalf("nonblocking ack = %d %s", r1.Code, r1.Body.String())
	}
	select {
	case <-putStarted:
	case <-time.After(10 * time.Second):
		t.Fatal("winner never reached upstream put")
	}
	r2 := httptest.NewRecorder()
	duplicateDone := make(chan struct{})
	go func() {
		h.handler.ServeHTTP(r2, mk(false))
		close(duplicateDone)
	}()
	select {
	case <-duplicateDone:
	case <-time.After(10 * time.Second):
		release()
		<-duplicateDone
		t.Fatal("duplicate completion did not reject while the winner was uploading")
	}

	var d2 map[string]any
	_ = json.Unmarshal(r2.Body.Bytes(), &d2)
	if r2.Code != http.StatusConflict {
		t.Fatalf("duplicate complete = %d %v, want 409 claim conflict", r2.Code, d2)
	}
	if d2["Status"] != store.RelayUploadUploading {
		t.Fatalf("duplicate did not report the in-progress receipt: %v", d2)
	}
	if id, _ := d2["UploadId"].(string); id != "chunked-up-race" {
		t.Fatalf("loser answer lacks the upload id: %v", d2)
	}

	release()

	// Exactly one truthful terminal state on the receipt, queryable.
	deadline := time.Now().Add(15 * time.Second)
	for {
		srec, sdoc := receiptStatusOf(t, h, relayPlain, "chunked-up-race")
		if srec.Code == 200 && sdoc["Status"] != store.RelayUploadUploading {
			if sdoc["Status"] != store.RelayUploadCompleted {
				t.Fatalf("race receipt state = %v (%v)", sdoc["Status"], sdoc["Message"])
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("race never finished: %d %v", srec.Code, sdoc)
		}
		time.Sleep(100 * time.Millisecond)
	}

	// EXACTLY ONE upstream put: the loser never touched the storage backend.
	h.fake.mu.Lock()
	puts := len(h.fake.puts)
	h.fake.mu.Unlock()
	if puts != 1 {
		t.Fatalf("duplicate completion reached upstream: %d puts, want exactly 1", puts)
	}
}

// TestGokapiReceiptMessageScrubbed proves credential-looking values are
// redacted from the response, the receipt and the audit row alike.
func TestGokapiReceiptMessageScrubbed(t *testing.T) {
	h := newHarness(t)
	h.addRelayKey(t, "relay", relayPlain, store.RelaySettings{})
	h.fake.mu.Lock()
	h.fake.putReject = true
	h.fake.putErrMsg = "upstream 401 Authorization: Bearer sk-c2a-abcd1234-ZeCrEtSeCrEtSeCrEt99 token=supersecret1 sign=deadbeefcafe"
	h.fake.mu.Unlock()

	rec, doc := gokapiUpload(t, h, relayPlain, map[string]string{"allowedDownloads": "0"}, "leak.bin", []byte("x"), "")
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("add = %d %v", rec.Code, doc)
	}
	id, _ := doc["UploadId"].(string)
	msg, _ := doc["Message"].(string)
	for _, secret := range []string{"ZeCrEt", "supersecret1", "deadbeefcafe"} {
		if strings.Contains(msg, secret) {
			t.Fatalf("response leaks %q: %s", secret, msg)
		}
	}
	if !strings.Contains(msg, "<redacted>") {
		t.Fatalf("response not scrubbed: %s", msg)
	}
	// Receipt row scrubbed identically.
	srec, sdoc := receiptStatusOf(t, h, relayPlain, id)
	if srec.Code != 200 {
		t.Fatalf("status = %d", srec.Code)
	}
	if rmsg, _ := sdoc["Message"].(string); strings.Contains(rmsg, "supersecret1") || strings.Contains(rmsg, "ZeCrEt") {
		t.Fatalf("receipt leaks: %s", rmsg)
	}
	// Audit row scrubbed too.
	entries, _, err := h.st.QueryAudit(store.AuditFilter{KeyExact: "relay", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Error, "supersecret1") || strings.Contains(e.Error, "ZeCrEt") {
			t.Fatalf("audit leaks: %s", e.Error)
		}
	}
}

// TestGokapiUploadStoreFailureNeverCompleted closes the store while the
// upstream put is in flight: the response may not claim completion when the
// terminal receipt (or the metadata) could not be persisted.
func TestGokapiUploadStoreFailureNeverCompleted(t *testing.T) {
	h := newHarness(t)
	h.addRelayKey(t, "relay", relayPlain, store.RelaySettings{})
	h.fake.mu.Lock()
	h.fake.putSleep = 400 * time.Millisecond
	h.fake.mu.Unlock()

	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		rec, _ := gokapiUploadRaw(t, h, relayPlain, map[string]string{"allowedDownloads": "0"}, "slow.bin", []byte("slow"), "", nil)
		done <- rec
	}()
	time.Sleep(120 * time.Millisecond) // receipt reserved, put still streaming
	h.st.Close()
	rec := <-done
	var doc map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &doc)
	if rec.Code == 200 && doc["Status"] == store.RelayUploadCompleted {
		t.Fatalf("claimed completion despite closed store: %d %v", rec.Code, doc)
	}
	if msg, _ := doc["Message"].(string); !strings.Contains(msg, "unconfirmed") && !strings.Contains(msg, "could not") && !strings.Contains(msg, "partial or complete object") {
		t.Fatalf("message not honest about uncertainty: %d %q", rec.Code, msg)
	}
}

// TestGokapiUploadTaskIDNeverCompletesEarly pins the authoritative-completion
// rule for queued tasks: a put answered by a NESTED data.task (the patched
// fork's shape) is only completed once the task is tracked to success; an
// untrackable task stays unconfirmed.
func TestGokapiUploadTaskIDNeverCompletesEarly(t *testing.T) {
	h := newHarness(t)
	h.addRelayKey(t, "relay", relayPlain, store.RelaySettings{})

	// Queued task (nested data.task.id) that completes shortly: the upload
	// completes only after the task is observed as done.
	h.fake.mu.Lock()
	h.fake.putAsyncTask = true
	h.fake.taskDelay = 150 * time.Millisecond
	h.fake.mu.Unlock()
	rec, doc := gokapiUpload(t, h, relayPlain, map[string]string{"allowedDownloads": "0"}, "queued.bin", []byte("queued"), "")
	if rec.Code != 200 || doc["Status"] != store.RelayUploadCompleted {
		t.Fatalf("queued-task upload = %d %v", rec.Code, doc)
	}

	// awaitRelayUploadTask with a task that never resolves: the outcome is
	// unconfirmed, never success.
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	gerr := h.api.awaitRelayUploadTask(ctx, "t-nope")
	if gerr == nil || gerr.status != http.StatusGatewayTimeout || !strings.Contains(gerr.msg, "verify") {
		t.Fatalf("unresolved task = %+v, want unconfirmed guidance", gerr)
	}
}

// TestGokapiRelayKeyIsolation pins the per-key restriction of the relay file
// endpoints: foreign ids are indistinguishable 404s, and deleting (or
// restoring) relay files requires the explicit delete permission. The keyless
// public share routes stay deliberately open.
func TestGokapiRelayKeyIsolation(t *testing.T) {
	h := newHarness(t)
	// Owner WITHOUT the delete permission (the common preset shape).
	h.addRelayKey(t, "owner", relayPlain, store.RelaySettings{}, func(k *store.APIKey) {
		k.Drives[0].Perms = []string{"list", "read", "write"}
	})
	otherPlain := "sk-c2a-re1ay003-relaysecretrelaysecre3"
	h.addRelayKey(t, "other", otherPlain, store.RelaySettings{})

	rec, doc := gokapiUpload(t, h, relayPlain, map[string]string{"allowedDownloads": "5", "expiryDays": "7"}, "mine.bin", []byte("mine"), "")
	if rec.Code != 200 {
		t.Fatalf("upload = %d %v", rec.Code, doc)
	}
	fileID, _ := fileInfoOf(t, doc)["Id"].(string)
	if fileID == "" {
		t.Fatal("no file id")
	}

	// The foreign key cannot see, download, delete or restore the owner's id.
	for _, tc := range []struct {
		name, method, target string
		hdr                  map[string]string
	}{
		{"list one", http.MethodGet, "/api/files/list/" + fileID, nil},
		{"download", http.MethodGet, "/api/files/download/" + fileID, nil},
		{"delete", http.MethodDelete, "/api/files/delete", map[string]string{"id": fileID}},
		{"restore", http.MethodPost, "/api/files/restore", map[string]string{"id": fileID}},
	} {
		rec, doc := h.do(tc.method, tc.target, otherPlain, nil, tc.hdr)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s by foreign key = %d %v, want 404", tc.name, rec.Code, doc)
		}
	}

	// The owner itself cannot delete or restore without the delete perm.
	rec, doc = h.do(http.MethodDelete, "/api/files/delete", relayPlain, nil, map[string]string{"id": fileID})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("delete without delete perm = %d %v, want 403", rec.Code, doc)
	}
	// Arming a delayed deletion is the same destructive capability.
	rec, _ = h.do(http.MethodDelete, "/api/files/delete", relayPlain, nil, map[string]string{"id": fileID, "delay": "60"})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("delayed delete without delete perm = %d, want 403", rec.Code)
	}
	rec, _ = h.do(http.MethodPost, "/api/files/restore", relayPlain, nil, map[string]string{"id": fileID})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("restore without delete perm = %d, want 403", rec.Code)
	}

	// Granting delete unlocks deletion of the owner's own file.
	k, _ := h.st.GetKeyWithGrants("owner")
	k.Drives[0].Perms = append(k.Drives[0].Perms, "delete")
	if err := h.st.UpdateKey(k); err != nil {
		t.Fatal(err)
	}
	if err := h.keys.Reload(); err != nil {
		t.Fatal(err)
	}
	rec, _ = h.do(http.MethodDelete, "/api/files/delete", relayPlain, nil, map[string]string{"id": fileID})
	if rec.Code != http.StatusOK {
		t.Fatalf("delete with delete perm = %d %s", rec.Code, rec.Body.String())
	}

	// The keyless public routes remain deliberately open (armed with a fresh
	// upload; the deleted file's id must be gone).
	rec, doc = gokapiUpload(t, h, relayPlain, map[string]string{"allowedDownloads": "3", "expiryDays": "7"}, "pub.bin", []byte("pub"), "")
	pubID, _ := fileInfoOf(t, doc)["Id"].(string)
	req := httptest.NewRequest(http.MethodGet, "/downloadFile?id="+pubID, nil)
	prec := httptest.NewRecorder()
	h.handler.ServeHTTP(prec, req)
	if prec.Code != 200 || prec.Body.String() != "pub" {
		t.Fatalf("public download = %d %q", prec.Code, prec.Body.String())
	}
}
