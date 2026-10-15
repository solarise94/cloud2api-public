package goapiclient

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRawUploadPreservesOpaqueReaderContentLength(t *testing.T) {
	const content = "hello bytes"
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut || r.ContentLength != int64(len(content)) || len(r.TransferEncoding) != 0 {
			t.Errorf("opaque upload lost its explicit length: method=%s length=%d transfer=%v", r.Method, r.ContentLength, r.TransferEncoding)
			w.WriteHeader(http.StatusLengthRequired)
			return
		}
		data, err := io.ReadAll(r.Body)
		if err != nil || string(data) != content {
			t.Errorf("uploaded body = %q, %v", data, err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"path":"/hello.txt","size":11,"crc64":"0","sha256":"synthetic-hash","conflict":"created"}`)
	}))
	defer ts.Close()
	client, err := New(ts.URL, "synthetic-upstream-key")
	if err != nil {
		t.Fatal(err)
	}
	// NewRequest cannot infer a length from this wrapper, like the driver's
	// progress and checksum readers. The explicit protocol length must survive.
	body := io.LimitReader(strings.NewReader(content), int64(len(content)))
	if _, err := client.Upload(t.Context(), "d1", "/hello.txt", "hello.txt", "fail", int64(len(content)), body); err != nil {
		t.Fatal(err)
	}
}
