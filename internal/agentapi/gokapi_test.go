package agentapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"hash/crc64"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cloud2api/internal/store"
)

// relayPlain is a dedicated relay test key (drive d1, chroot /agents/r).
const relayPlain = "sk-c2a-re1ay001-relaysecretrelaysecret99"

// addRelayKey registers a key with the global relay permission.
func (h *harness) addRelayKey(t *testing.T, id, plain string, relay store.RelaySettings, mut ...func(*store.APIKey)) {
	t.Helper()
	k := store.APIKey{
		ID:          id,
		Hash:        hashOf(plain),
		CreatedAt:   time.Now().UTC(),
		Drives:      []store.KeyDrive{{Drive: "d1", Root: "/agents/r", Perms: []string{"list", "read", "write", "delete"}}},
		GlobalPerms: []string{"relay"},
		Trash:       store.TrashSettings{Enabled: true, Days: 7},
		Relay:       relay,
	}
	if pub, _, ok := splitNew(plain); ok {
		k.PublicID = pub
	}
	for _, m := range mut {
		m(&k)
	}
	if err := h.st.InsertKey(k); err != nil {
		t.Fatal(err)
	}
	if err := h.keys.Reload(); err != nil {
		t.Fatal(err)
	}
}

// multipartBody builds one multipart/form-data body with scalar fields plus a
// `file` part.
func multipartBody(t *testing.T, fields map[string]string, filename string, content []byte, contentType string) (string, []byte) {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	for k, v := range fields {
		if err := w.WriteField(k, v); err != nil {
			t.Fatal(err)
		}
	}
	h := make(textproto.MIMEHeader)
	h.Set("Content-Disposition", fmt.Sprintf(`form-data; name="file"; filename=%q`, filename))
	if contentType != "" {
		h.Set("Content-Type", contentType)
	}
	part, err := w.CreatePart(h)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return w.FormDataContentType(), buf.Bytes()
}

// gokapiUpload uploads content through /api/files/add and returns the parsed
// response plus its recorder.
func gokapiUpload(t *testing.T, h *harness, key string, fields map[string]string, filename string, content []byte, contentType string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	ct, body := multipartBody(t, fields, filename, content, contentType)
	req := httptest.NewRequest(http.MethodPost, "/api/files/add", bytes.NewReader(body))
	req.Header.Set("apikey", key)
	req.Header.Set("Content-Type", ct)
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	var doc map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &doc)
	return rec, doc
}

func fileInfoOf(t *testing.T, doc map[string]any) map[string]any {
	t.Helper()
	fi, ok := doc["FileInfo"].(map[string]any)
	if !ok {
		t.Fatalf("no FileInfo in %v", doc)
	}
	return fi
}

func localCRC64(data []byte) string {
	c := crc64.New(crc64Table)
	c.Write(data)
	return u64decimal(c.Sum64())
}

// ---- version / config / shape ----

func TestGokapiVersionAndConfig(t *testing.T) {
	h := newHarness(t)
	h.addRelayKey(t, "relay", relayPlain, store.RelaySettings{})

	rec, doc := h.do(http.MethodGet, "/api/info/version", relayPlain, nil, nil)
	if rec.Code != 200 || doc["Version"] != gokapiCompatVersion || doc["VersionInt"] != float64(gokapiCompatVersionInt) || doc["EndToEndEncryptionEnabled"] != false {
		t.Fatalf("version = %d %v", rec.Code, doc)
	}
	rec, doc = h.do(http.MethodGet, "/api/info/config", relayPlain, nil, nil)
	if rec.Code != 200 || doc["MaxFilesize"] != float64(8192) || doc["MaxChunksize"] != float64(relayChunkMiB) {
		t.Fatalf("config = %d %v", rec.Code, doc)
	}
	// No key -> 401; a key without the relay permission may still read the
	// version but not the config.
	rec, _ = h.do(http.MethodGet, "/api/info/version", "", nil, nil)
	if rec.Code != 401 {
		t.Fatalf("no key version = %d", rec.Code)
	}
	rec, _ = h.do(http.MethodGet, "/api/info/config", chrootPlain, nil, nil)
	if rec.Code != 403 {
		t.Fatalf("config without relay perm = %d", rec.Code)
	}
	// Bearer authentication also works.
	req := httptest.NewRequest(http.MethodGet, "/api/info/version", nil)
	req.Header.Set("Authorization", "Bearer "+relayPlain)
	rec2 := httptest.NewRecorder()
	h.handler.ServeHTTP(rec2, req)
	if rec2.Code != 200 {
		t.Fatalf("bearer = %d", rec2.Code)
	}
}

// TestGokapiFileInfoGolden pins the exact JSON shape (field names, order and
// types) of the Gokapi File object the relay returns.
func TestGokapiFileInfoGolden(t *testing.T) {
	h := newHarness(t)
	uploaded := time.Unix(1700000000, 0).UTC()
	expire := uploaded.Add(7 * 24 * time.Hour).Truncate(time.Second)
	f := store.RelayFile{
		ID: "oNleRD3pUZgaDKn", HotlinkID: "tDMs0U8MvRFwK69PfjagI7F87C13UVeQuOGDvtCG.jpg",
		KeyID: "relay", Drive: "d1", Name: "Test File.jpg", Size: 1394,
		CRC64: "11051210869376104954", SHA256: strings.Repeat("a", 64),
		ContentType: "image/jpeg", UploadedAt: uploaded, ExpireAt: expire,
		DownloadsRemaining: 4, DownloadCount: 1,
	}
	got, err := json.Marshal(h.api.gokapiFileInfoOf("https://go.example.com", f))
	if err != nil {
		t.Fatal(err)
	}
	want := `{"Id":"oNleRD3pUZgaDKn","Name":"Test File.jpg","Size":"1.4 kB","SizeBytes":1394,` +
		`"CRC64":"11051210869376104954","SHA256":"` + strings.Repeat("a", 64) + `",` +
		`"HotlinkId":"tDMs0U8MvRFwK69PfjagI7F87C13UVeQuOGDvtCG.jpg","ContentType":"image/jpeg",` +
		`"ExpireAtString":"2023-11-21 22:13","UrlDownload":"https://go.example.com/d?id=oNleRD3pUZgaDKn",` +
		`"UrlHotlink":"https://go.example.com/h/tDMs0U8MvRFwK69PfjagI7F87C13UVeQuOGDvtCG.jpg",` +
		`"FileRequestId":"","UploadDate":1700000000,"ExpireAt":1700604800,` +
		`"DownloadsRemaining":4,"DownloadCount":1,"UnlimitedDownloads":false,"UnlimitedTime":false,` +
		`"RequiresClientSideDecryption":false,"IsEncrypted":false,"IsEndToEndEncrypted":false,` +
		`"IsPasswordProtected":false,"IsSavedOnLocalStorage":false,"IsPendingDeletion":false,` +
		`"IsFileRequest":false,"UploaderId":1}`
	if string(got) != want {
		t.Fatalf("FileInfo shape mismatch:\n got: %s\nwant: %s", got, want)
	}
	// Unlimited + pending deletion variants.
	f.DownloadsRemaining = -1
	f.PendingDeletion = uploaded
	got, _ = json.Marshal(h.api.gokapiFileInfoOf("https://go.example.com", f))
	for _, want := range []string{`"UnlimitedDownloads":true`, `"DownloadsRemaining":0`, `"IsPendingDeletion":true`} {
		if !strings.Contains(string(got), want) {
			t.Errorf("unlimited/pending variant lacks %s: %s", want, got)
		}
	}
}

// ---- upload / verify (CRC64 decimal string, sizes) ----

func TestGokapiUploadVerifyCRC64(t *testing.T) {
	h := newHarness(t)
	h.addRelayKey(t, "relay", relayPlain, store.RelaySettings{})

	// The CRC-64/XZ reference vector must come back as a decimal STRING.
	content := []byte("123456789")
	rec, doc := gokapiUpload(t, h, relayPlain, map[string]string{"allowedDownloads": "0", "expiryDays": "7"}, "check.bin", content, "application/octet-stream")
	if rec.Code != 200 || doc["Result"] != "OK" {
		t.Fatalf("add = %d %v", rec.Code, doc)
	}
	fi := fileInfoOf(t, doc)
	if fi["CRC64"] != "11051210869376104954" {
		t.Fatalf("CRC64 = %v (%T), want the decimal string", fi["CRC64"], fi["CRC64"])
	}
	if fi["SizeBytes"] != float64(len(content)) || fi["Name"] != "check.bin" {
		t.Fatalf("FileInfo = %v", fi)
	}
	if id, _ := fi["Id"].(string); len(id) != relayFileIDLen {
		t.Fatalf("Id = %v, want %d random chars", fi["Id"], relayFileIDLen)
	}
	if ud, _ := fi["UrlDownload"].(string); !strings.Contains(ud, "/d?id=") {
		t.Fatalf("UrlDownload = %v", ud)
	}
	if uh, _ := fi["UrlHotlink"].(string); !strings.Contains(uh, "/h/") {
		t.Fatalf("UrlHotlink = %v", uh)
	}

	// A different payload verifies against the locally computed CRC.
	payload := []byte("relay acceptance payload 0123456789")
	rec, doc = gokapiUpload(t, h, relayPlain, map[string]string{"allowedDownloads": "0", "expiryDays": "7"}, "p.bin", payload, "")
	if rec.Code != 200 {
		t.Fatalf("second add = %d %v", rec.Code, doc)
	}
	if got := fileInfoOf(t, doc)["CRC64"]; got != localCRC64(payload) {
		t.Fatalf("CRC64 %v != local %s", got, localCRC64(payload))
	}
	if ct := fileInfoOf(t, doc)["ContentType"]; ct != "application/octet-stream" {
		t.Fatalf("ContentType = %v", ct)
	}

	// Password-protected uploads are rejected.
	rec, doc = gokapiUpload(t, h, relayPlain, map[string]string{"password": "hunter2"}, "p.bin", payload, "")
	if rec.Code != 400 {
		t.Fatalf("password = %d %v, want 400", rec.Code, doc)
	}
	// Missing Content-Length is rejected with 411.
	req := httptest.NewRequest(http.MethodPost, "/api/files/add", nil)
	req.Header.Set("apikey", relayPlain)
	req.ContentLength = -1
	req.TransferEncoding = []string{"chunked"}
	rec2 := httptest.NewRecorder()
	h.handler.ServeHTTP(rec2, req)
	if rec2.Code != 411 {
		t.Fatalf("chunked = %d, want 411", rec2.Code)
	}
}

// ---- expiry cap ----

func TestGokapiExpiryCap(t *testing.T) {
	h := newHarness(t)
	// max_age_days=2 caps expiryDays=7 (and 0 -> the cap).
	h.addRelayKey(t, "relay", relayPlain, store.RelaySettings{MaxAgeDays: 2})

	for _, days := range []string{"7", "0", ""} {
		rec, doc := gokapiUpload(t, h, relayPlain, map[string]string{"allowedDownloads": "0", "expiryDays": days}, "cap.bin", []byte("x"), "")
		if rec.Code != 200 {
			t.Fatalf("expiryDays=%q add = %d %v", days, rec.Code, doc)
		}
		fi := fileInfoOf(t, doc)
		got := time.Unix(int64(fi["ExpireAt"].(float64)), 0)
		wantLow := time.Now().Add(2*24*time.Hour - time.Minute)
		wantHigh := time.Now().Add(2*24*time.Hour + time.Minute)
		if got.Before(wantLow) || got.After(wantHigh) {
			t.Fatalf("expiryDays=%q expire = %v, want ~now+2d", days, got)
		}
		if fi["UnlimitedTime"] != false {
			t.Fatalf("UnlimitedTime = %v, retention is always capped", fi["UnlimitedTime"])
		}
		// cleanup for the next iteration
		h.do(http.MethodDelete, "/api/files/delete", relayPlain, nil, map[string]string{"id": fi["Id"].(string)})
	}

	// Default key: 0 -> 7 days.
	h.addRelayKey(t, "relay2", "sk-c2a-re1ay002-relaysecretrelaysecre98", store.RelaySettings{})
	rec, doc := gokapiUpload(t, h, "sk-c2a-re1ay002-relaysecretrelaysecre98", map[string]string{"allowedDownloads": "0"}, "cap.bin", []byte("x"), "")
	if rec.Code != 200 {
		t.Fatalf("default add = %d %v", rec.Code, doc)
	}
	got := time.Unix(int64(fileInfoOf(t, doc)["ExpireAt"].(float64)), 0)
	if d := time.Until(got); d < 6*24*time.Hour || d > 8*24*time.Hour {
		t.Fatalf("default expiry = %v (~%v), want ~7d", got, d)
	}
}

// ---- downloads: exhaustion + unlimited ----

func TestGokapiDownloadAndExhaustion(t *testing.T) {
	h := newHarness(t)
	h.addRelayKey(t, "relay", relayPlain, store.RelaySettings{})

	payload := []byte("downloadable-payload-42")
	rec, doc := gokapiUpload(t, h, relayPlain, map[string]string{"allowedDownloads": "1", "expiryDays": "7"}, "once.bin", payload, "")
	if rec.Code != 200 {
		t.Fatalf("add = %d %v", rec.Code, doc)
	}
	fi := fileInfoOf(t, doc)
	id := fi["Id"].(string)
	hotlink := strings.TrimPrefix(fi["UrlHotlink"].(string), "http://example.com")

	// The authenticated API download works without consuming the counter.
	rec, _ = h.do(http.MethodGet, "/api/files/download/"+id, relayPlain, nil, nil)
	if rec.Code != 200 || rec.Body.String() != string(payload) {
		t.Fatalf("api download = %d %q", rec.Code, rec.Body.String())
	}
	// increaseCounter=true consumes it.
	rec, _ = h.do(http.MethodGet, "/api/files/download/"+id, relayPlain, nil, map[string]string{"increaseCounter": "true"})
	if rec.Code != 200 {
		t.Fatalf("increaseCounter download = %d", rec.Code)
	}

	// Public hotlink: exhausted -> 410.
	req := httptest.NewRequest(http.MethodGet, hotlink, nil)
	rec2 := httptest.NewRecorder()
	h.handler.ServeHTTP(rec2, req)
	if rec2.Code != 410 {
		t.Fatalf("exhausted hotlink = %d, want 410", rec2.Code)
	}
	// /downloadFile also exhausted.
	rec, _ = h.do(http.MethodGet, "/downloadFile?id="+id, "", nil, nil)
	if rec.Code != 410 {
		t.Fatalf("exhausted downloadFile = %d, want 410", rec.Code)
	}
	// Unknown ids -> 404.
	rec, _ = h.do(http.MethodGet, "/downloadFile?id=doesnotexist", "", nil, nil)
	if rec.Code != 404 {
		t.Fatalf("unknown downloadFile = %d", rec.Code)
	}

	// Unlimited downloads stay downloadable; DownloadCount grows.
	rec, doc = gokapiUpload(t, h, relayPlain, map[string]string{"allowedDownloads": "0", "expiryDays": "7"}, "inf.bin", payload, "")
	fi = fileInfoOf(t, doc)
	id2 := fi["Id"].(string)
	if fi["UnlimitedDownloads"] != true || fi["DownloadsRemaining"] != float64(0) {
		t.Fatalf("unlimited FileInfo = %v", fi)
	}
	for i := 0; i < 3; i++ {
		rec, _ = h.do(http.MethodGet, "/downloadFile?id="+id2, "", nil, nil)
		if rec.Code != 200 || rec.Body.String() != string(payload) {
			t.Fatalf("unlimited download %d = %d", i, rec.Code)
		}
	}
	rec, doc = h.do(http.MethodGet, "/api/files/list/"+id2, relayPlain, nil, nil)
	if rec.Code != 200 || doc["DownloadCount"] != float64(3) {
		t.Fatalf("count = %v", doc)
	}

	// The share page renders filename/size and the download button.
	rec, _ = h.do(http.MethodGet, "/d?id="+id2, "", nil, nil)
	if rec.Code != 200 {
		t.Fatalf("share page = %d", rec.Code)
	}
	page := rec.Body.String()
	for _, want := range []string{"inf.bin", "downloadFile?id=" + id2, "23 B"} {
		if !strings.Contains(page, want) {
			t.Errorf("share page lacks %q", want)
		}
	}
}

// ---- list / delete / restore ----

func TestGokapiListDeleteRestore(t *testing.T) {
	h := newHarness(t)
	h.addRelayKey(t, "relay", relayPlain, store.RelaySettings{})

	// Empty list is JSON null.
	rec, doc := h.do(http.MethodGet, "/api/files/list", relayPlain, nil, nil)
	if rec.Code != 200 || rec.Body.String() != "null\n" {
		t.Fatalf("empty list = %d %q", rec.Code, rec.Body.String())
	}

	rec, doc = gokapiUpload(t, h, relayPlain, map[string]string{"allowedDownloads": "0", "expiryDays": "7"}, "del.bin", []byte("bye"), "")
	id := fileInfoOf(t, doc)["Id"].(string)

	// List contains the file (a bare JSON array).
	rec, doc = h.do(http.MethodGet, "/api/files/list", relayPlain, nil, nil)
	var files []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &files); err != nil || len(files) != 1 || files[0]["Id"] != id {
		t.Fatalf("list = %s (err %v)", rec.Body.String(), err)
	}

	// Delayed deletion: pending, restorable, still downloadable meanwhile.
	rec, _ = h.do(http.MethodDelete, "/api/files/delete", relayPlain, nil, map[string]string{"id": id, "delay": "3600"})
	if rec.Code != 200 {
		t.Fatalf("delayed delete = %d", rec.Code)
	}
	rec, doc = h.do(http.MethodGet, "/api/files/list/"+id, relayPlain, nil, nil)
	if rec.Code != 200 || doc["IsPendingDeletion"] != true {
		t.Fatalf("pending view = %d %v", rec.Code, doc)
	}
	rec, _ = h.do(http.MethodGet, "/downloadFile?id="+id, "", nil, nil)
	if rec.Code != 200 {
		t.Fatalf("pending file not downloadable = %d", rec.Code)
	}
	// Restore cancels the pending deletion.
	rec, doc = h.do(http.MethodPost, "/api/files/restore", relayPlain, nil, map[string]string{"id": id})
	if rec.Code != 200 || doc["Result"] != "OK" {
		t.Fatalf("restore = %d %v", rec.Code, doc)
	}
	rec, _ = h.do(http.MethodGet, "/api/files/list/"+id, relayPlain, nil, nil)
	if rec.Code != 200 {
		t.Fatalf("after restore = %d", rec.Code)
	}
	// Restoring a non-pending file -> 404.
	rec, _ = h.do(http.MethodPost, "/api/files/restore", relayPlain, nil, map[string]string{"id": id})
	if rec.Code != 404 {
		t.Fatalf("restore non-pending = %d, want 404", rec.Code)
	}

	// Immediate deletion removes the file and its bytes.
	rec, _ = h.do(http.MethodDelete, "/api/files/delete", relayPlain, nil, map[string]string{"id": id})
	if rec.Code != 200 {
		t.Fatalf("delete = %d", rec.Code)
	}
	rec, _ = h.do(http.MethodGet, "/api/files/list/"+id, relayPlain, nil, nil)
	if rec.Code != 404 {
		t.Fatalf("deleted metadata = %d, want 404", rec.Code)
	}
	rec, _ = h.do(http.MethodGet, "/downloadFile?id="+id, "", nil, nil)
	if rec.Code != 404 {
		t.Fatalf("deleted download = %d, want 404", rec.Code)
	}
	// Unknown id delete -> 404; missing id header -> 400.
	rec, _ = h.do(http.MethodDelete, "/api/files/delete", relayPlain, nil, map[string]string{"id": "nope"})
	if rec.Code != 404 {
		t.Fatalf("delete unknown = %d", rec.Code)
	}
	rec, _ = h.do(http.MethodDelete, "/api/files/delete", relayPlain, nil, nil)
	if rec.Code != 400 {
		t.Fatalf("delete no id = %d", rec.Code)
	}
}

// ---- quota + eviction ----

func TestGokapiQuotaEviction(t *testing.T) {
	h := newHarness(t)
	// Tiny quota: 10 bytes -> three 4-byte files cannot coexist.
	h.addRelayKey(t, "relay", relayPlain, store.RelaySettings{QuotaBytes: 10})

	// A non-relay file inside the same chroot must never be evicted.
	h.do(http.MethodPut, "/v1/files?path=/keep.bin", relayPlain, []byte("keep"), map[string]string{"Content-Type": "application/octet-stream"})

	ids := []string{}
	for i, name := range []string{"a.bin", "b.bin", "c.bin"} {
		rec, doc := gokapiUpload(t, h, relayPlain, map[string]string{"allowedDownloads": "0", "expiryDays": "7"}, name, []byte(fmt.Sprintf("data%d", i)), "")
		if rec.Code != 200 {
			t.Fatalf("upload %s = %d %v", name, rec.Code, doc)
		}
		ids = append(ids, fileInfoOf(t, doc)["Id"].(string))
	}
	// usage would be 5+5+5=15 > 10 -> the oldest (a.bin) was evicted, leaving
	// b.bin + c.bin (10 bytes).
	rec, doc := h.do(http.MethodGet, "/api/files/list", relayPlain, nil, nil)
	var files []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &files); err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 || files[0]["Id"] != ids[1] || files[1]["Id"] != ids[2] {
		t.Fatalf("survivors = %v (ids=%v)", files, ids)
	}
	if n, used, _ := h.st.RelayFileCounts("relay"); n != 2 || used != 10 {
		t.Fatalf("relay counts = %d files / %d bytes, want 2 / 10", n, used)
	}
	// The evicted file is gone.
	if _, err := h.st.GetRelayFile(ids[0]); err == nil {
		t.Fatal("oldest file still has metadata")
	}
	h.fake.mu.Lock()
	evictedPath := "/m1/agents/r/" + relayDirName + "/" + ids[0]
	_, evictedBytes := h.fake.entries[evictedPath]
	h.fake.mu.Unlock()
	if evictedBytes {
		t.Fatalf("evicted bytes still upstream at %s", evictedPath)
	}
	// The non-relay file is untouched.
	rec, _ = h.do(http.MethodGet, "/v1/stat?path=/keep.bin", relayPlain, nil, nil)
	if rec.Code != 200 {
		t.Fatalf("non-relay file evicted: %d", rec.Code)
	}

	// An upload larger than the whole quota fails and leaves nothing behind.
	rec, doc = gokapiUpload(t, h, relayPlain, map[string]string{"allowedDownloads": "0", "expiryDays": "7"}, "huge.bin", bytes.Repeat([]byte("x"), 40), "")
	if rec.Code != 400 {
		t.Fatalf("oversized upload = %d %v, want 400", rec.Code, doc)
	}
	if n, _, _ := h.st.RelayFileCounts("relay"); n != 2 {
		t.Fatalf("rows after oversized upload = %d, want 2", n)
	}
}

// ---- chunk upload ----

func TestGokapiChunkUpload(t *testing.T) {
	h := newHarness(t)
	h.addRelayKey(t, "relay", relayPlain, store.RelaySettings{})

	// 6 MiB payload: one 5 MiB chunk + the (small) final chunk.
	payload := bytes.Repeat([]byte{0xA5}, 6<<20)
	payload[len(payload)-1] = 0x5A
	chunk1 := payload[:5<<20]
	chunk2 := payload[5<<20:]

	chunkAdd := func(uuid string, filesize int64, offset int64, data []byte) *httptest.ResponseRecorder {
		t.Helper()
		ct, body := multipartBody(t, map[string]string{
			"uuid":     uuid,
			"filesize": fmt.Sprintf("%d", filesize),
			"offset":   fmt.Sprintf("%d", offset),
		}, "chunk", data, "application/octet-stream")
		req := httptest.NewRequest(http.MethodPost, "/api/chunk/add", bytes.NewReader(body))
		req.Header.Set("apikey", relayPlain)
		req.Header.Set("Content-Type", ct)
		rec := httptest.NewRecorder()
		h.handler.ServeHTTP(rec, req)
		return rec
	}
	if rec := chunkAdd("uuid-1234567890", int64(len(payload)), 0, chunk1); rec.Code != 200 {
		t.Fatalf("chunk1 = %d %s", rec.Code, rec.Body.String())
	}

	// Validation: a short non-final chunk is rejected.
	if rec := chunkAdd("uuid-1234567890", int64(len(payload)), 5<<20, chunk2[:64]); rec.Code != 400 {
		t.Fatalf("short chunk = %d, want 400", rec.Code)
	}
	// A chunk beyond the declared size is rejected.
	if rec := chunkAdd("uuid-1234567890", int64(len(payload)), 5<<20, append(append([]byte{}, chunk2...), 0)); rec.Code != 400 {
		t.Fatalf("overflow chunk = %d, want 400", rec.Code)
	}
	// Short uuid is rejected.
	if rec := chunkAdd("short", int64(len(payload)), 0, chunk1); rec.Code != 400 {
		t.Fatalf("short uuid = %d, want 400", rec.Code)
	}
	// Changing the declared filesize mid-upload is rejected.
	if rec := chunkAdd("uuid-1234567890", int64(len(payload))+99, 5<<20, chunk2); rec.Code != 400 {
		t.Fatalf("changed filesize = %d, want 400", rec.Code)
	}

	// Final chunk + blocking completion -> FileInfo with the exact CRC64.
	if rec := chunkAdd("uuid-1234567890", int64(len(payload)), 5<<20, chunk2); rec.Code != 200 {
		t.Fatalf("chunk2 = %d %s", rec.Code, rec.Body.String())
	}
	req := httptest.NewRequest(http.MethodPost, "/api/chunk/complete", nil)
	req.Header.Set("apikey", relayPlain)
	req.Header.Set("uuid", "uuid-1234567890")
	req.Header.Set("filename", "big upload.bin")
	req.Header.Set("filesize", fmt.Sprintf("%d", len(payload)))
	req.Header.Set("contenttype", "application/x-test")
	req.Header.Set("allowedDownloads", "0")
	req.Header.Set("expiryDays", "7")
	req.Header.Set("nonblocking", "false")
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	var doc map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &doc)
	if rec.Code != 200 || doc["Result"] != "OK" {
		t.Fatalf("complete = %d %v", rec.Code, doc)
	}
	fi := fileInfoOf(t, doc)
	if fi["CRC64"] != localCRC64(payload) || fi["SizeBytes"] != float64(len(payload)) {
		t.Fatalf("chunked FileInfo = %v", fi)
	}
	if fi["Name"] != "big upload.bin" || fi["ContentType"] != "application/x-test" {
		t.Fatalf("chunked name/type = %v", fi)
	}
	// The staging directory is cleaned after completion.
	if _, err := os.Stat(filepath.Join(h.api.relayChunkRoot(), "uuid-1234567890")); !os.IsNotExist(err) {
		t.Fatal("staging dir survived completion")
	}
	// A second completion of the same uuid fails (uuid unknown).
	rec, _ = h.do(http.MethodPost, "/api/chunk/complete", relayPlain, nil, map[string]string{"uuid": "uuid-1234567890"})
	if rec.Code != 400 {
		t.Fatalf("re-complete = %d, want 400", rec.Code)
	}

	// A gap (missing middle chunk) fails completion.
	p3 := bytes.Repeat([]byte{7}, 11<<20)
	if rec := chunkAdd("uuid-gap-1234567", int64(len(p3)), 0, p3[:5<<20]); rec.Code != 200 {
		t.Fatalf("gap chunk1 = %d", rec.Code)
	}
	if rec := chunkAdd("uuid-gap-1234567", int64(len(p3)), 6<<20, p3[6<<20:]); rec.Code != 200 {
		t.Fatalf("gap chunk2 = %d", rec.Code)
	}
	rec, _ = h.do(http.MethodPost, "/api/chunk/complete", relayPlain, nil, map[string]string{
		"uuid": "uuid-gap-1234567", "filename": "gap.bin", "filesize": fmt.Sprintf("%d", len(p3))})
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "contiguous") {
		t.Fatalf("gap complete = %d %s, want 400 contiguity error", rec.Code, rec.Body.String())
	}

	// base64:-prefixed filenames decode.
	if rec := chunkAdd("uuid-b64-1234567", 8, 0, []byte("b64data!")); rec.Code != 200 {
		t.Fatalf("b64 chunk = %d", rec.Code)
	}
	rec, doc = h.do(http.MethodPost, "/api/chunk/complete", relayPlain, nil, map[string]string{
		"uuid": "uuid-b64-1234567", "filename": "base64:ZmlsZW5hbWUudHh0", "filesize": "8"})
	if rec.Code != 200 || fileInfoOf(t, doc)["Name"] != "filename.txt" {
		t.Fatalf("b64 complete = %d %v", rec.Code, doc)
	}
}

// ---- unsupported endpoints ----

func TestGokapiUnsupportedEndpoints403(t *testing.T) {
	h := newHarness(t)
	h.addRelayKey(t, "relay", relayPlain, store.RelaySettings{})
	for _, target := range []struct{ method, path string }{
		{http.MethodPost, "/api/auth/create"},
		{http.MethodDelete, "/api/auth/delete"},
		{http.MethodPut, "/api/auth/modify"},
		{http.MethodPost, "/api/user/create"},
		{http.MethodPut, "/api/user/modify"},
		{http.MethodGet, "/api/logs/get"},
		{http.MethodGet, "/api/logs/systemStatus"},
		{http.MethodPost, "/api/uploadrequest/save"},
		{http.MethodGet, "/api/uploadrequest/list"},
		{http.MethodPost, "/api/files/duplicate"},
		{http.MethodPut, "/api/files/modify"},
		{http.MethodPut, "/api/files/replace"},
		{http.MethodPut, "/api/files/changeOwner"},
		{http.MethodGet, "/api/files/downloadzip"},
	} {
		rec, _ := h.do(target.method, target.path, relayPlain, nil, nil)
		if rec.Code != 403 {
			t.Errorf("%s %s = %d, want 403", target.method, target.path, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "not supported") {
			t.Errorf("%s %s body = %s", target.method, target.path, rec.Body.String())
		}
	}
	// Unknown /api/files/... route still answers 404 and the legacy surface
	// keeps flowing to the old gateway (a plain /api/unknown is not ours).
	rec, _ := h.do(http.MethodGet, "/api/files/invent", relayPlain, nil, nil)
	if rec.Code != 404 {
		t.Fatalf("unknown files route = %d", rec.Code)
	}
}

// ---- relay visibility on /v1 ----

func TestRelayHiddenFromV1(t *testing.T) {
	h := newHarness(t)
	h.addRelayKey(t, "relay", relayPlain, store.RelaySettings{})
	rec, doc := gokapiUpload(t, h, relayPlain, map[string]string{"allowedDownloads": "0", "expiryDays": "7"}, "hid.bin", []byte("x"), "")
	if rec.Code != 200 {
		t.Fatalf("add = %d %v", rec.Code, doc)
	}
	id := fileInfoOf(t, doc)["Id"].(string)

	// Not visible in /v1 listings of the root.
	_, doc = h.do(http.MethodGet, "/v1/list?path=/", relayPlain, nil, nil)
	for _, it := range doc["items"].([]any) {
		if name := it.(map[string]any)["name"]; name == relayDirName {
			t.Fatalf(".relay visible in /v1 listing: %v", doc)
		}
	}
	// Direct addressing is blocked.
	rec, doc = h.do(http.MethodGet, "/v1/stat?path=/"+relayDirName+"/"+id+"/hid.bin", relayPlain, nil, nil)
	if rec.Code != 400 || errCode(doc) != "path_invalid" {
		t.Fatalf("direct stat = %d %v, want 400 path_invalid", rec.Code, doc)
	}
	rec, _ = h.do(http.MethodGet, "/v1/list?path=/"+relayDirName, relayPlain, nil, nil)
	if rec.Code != 400 {
		t.Fatalf("direct list = %d, want 400", rec.Code)
	}
	// /v1/me exposes the relay settings and usage.
	_, doc = h.do(http.MethodGet, "/v1/me", relayPlain, nil, nil)
	relay := doc["relay"].(map[string]any)
	if relay["enabled"] != true || relay["drive"] != "d1" || relay["quota_bytes"] != float64(store.DefaultRelayQuota) || relay["max_age_days"] != float64(7) {
		t.Fatalf("me.relay = %v", relay)
	}
	if relay["files"] != float64(1) || relay["used_bytes"] != float64(1) {
		t.Fatalf("me.relay usage = %v", relay)
	}
}

// ---- retention sweep ----

func TestRelayRetentionSweep(t *testing.T) {
	h := newHarness(t)
	h.addRelayKey(t, "relay", relayPlain, store.RelaySettings{})

	rec, doc := gokapiUpload(t, h, relayPlain, map[string]string{"allowedDownloads": "0", "expiryDays": "7"}, "sweep.bin", []byte("gone"), "")
	if rec.Code != 200 {
		t.Fatalf("add = %d %v", rec.Code, doc)
	}
	id := fileInfoOf(t, doc)["Id"].(string)
	if _, err := h.st.GetRelayFile(id); err != nil {
		t.Fatal(err)
	}
	rec, _ = h.do(http.MethodGet, "/downloadFile?id="+id, "", nil, nil)
	if rec.Code != 200 {
		t.Fatalf("pre-sweep download = %d", rec.Code)
	}

	// Backdate the expiry (the documented test hook for retention).
	past := time.Now().UTC().Add(-time.Hour).Format("2006-01-02T15:04:05.000000000Z")
	if err := h.st.Exec(`UPDATE relay_files SET expire_at=? WHERE id=?`, past, id); err != nil {
		t.Fatal(err)
	}
	// An abandoned chunk staging dir older than a day is purged too.
	uuid := "uuid-abandoned-1"
	dir := filepath.Join(h.api.relayChunkRoot(), uuid)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "0"), []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}
	stale := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(filepath.Join(dir, "0"), stale, stale); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(dir, stale, stale); err != nil {
		t.Fatal(err)
	}
	// A fresh staging dir survives.
	fresh := filepath.Join(h.api.relayChunkRoot(), "uuid-fresh-12345")
	if err := os.MkdirAll(fresh, 0o700); err != nil {
		t.Fatal(err)
	}

	report := h.api.RunMaintenanceNow(context.Background())
	if report["relay_files_purged"] != 1 {
		t.Fatalf("report = %v", report)
	}
	if _, err := h.st.GetRelayFile(id); err == nil {
		t.Fatal("expired file row survived the sweep")
	}
	rec, _ = h.do(http.MethodGet, "/downloadFile?id="+id, "", nil, nil)
	if rec.Code != 404 {
		t.Fatalf("expired download = %d, want 404", rec.Code)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("abandoned chunk dir survived the sweep")
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Fatal("fresh chunk dir was purged")
	}
}

// ---- openapi coverage of the gokapi-compat tag ----

// TestGokapiOpenAPITag pins the gokapi-compat routes in the FULL protocol
// document (docs/openapi.full.json); the served basic profile carries none.
func TestGokapiOpenAPITag(t *testing.T) {
	h := newHarness(t)
	var doc struct {
		Paths map[string]map[string]any `json:"paths"`
	}
	if err := json.Unmarshal(marshalOpenAPIDoc(buildFullOpenAPI("http://example.com")), &doc); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"/api/info/version", "/api/info/config", "/api/files/add", "/api/files/list",
		"/api/files/list/{id}", "/api/files/delete", "/api/files/restore",
		"/api/files/download/{id}", "/api/chunk/add", "/api/chunk/complete",
		"/downloadFile", "/d", "/h/{hotlinkId}", "/custom/gokapi-agent-instructions.txt",
	} {
		item, ok := doc.Paths[want]
		if !ok {
			t.Errorf("openapi lacks %s", want)
			continue
		}
		for _, op := range item {
			m, _ := op.(map[string]any)
			if m == nil {
				continue
			}
			tags, _ := m["tags"].([]any)
			if len(tags) != 1 || tags[0] != "gokapi-compat" {
				t.Errorf("%s tags = %v, want [gokapi-compat]", want, tags)
			}
		}
	}
	// The Gokapi instructions asset renders with the request host and no key.
	rec2, _ := h.do(http.MethodGet, "/custom/gokapi-agent-instructions.txt", "", nil, nil)
	if rec2.Code != 200 {
		t.Fatalf("gokapi instructions = %d", rec2.Code)
	}
	body := rec2.Body.String()
	for _, want := range []string{
		"http://example.com", "/api/files/add", "allowedDownloads=0", "expiryDays=7",
		"11051210869376104954", "/downloadFile?id=", "swap the Base URL and API key",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("gokapi instructions lack %q", want)
		}
	}
	if strings.Contains(body, relayPlain) || strings.Contains(body, chrootPlain) {
		t.Error("gokapi instructions leak a key")
	}
}
