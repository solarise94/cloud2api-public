package openlist

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestDoDownloadClosesStalledRedirectBody(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/d/file" {
			w.Header().Set("Location", "/bytes")
			w.Header().Set("Content-Length", "4096")
			w.WriteHeader(http.StatusFound)
			w.(http.Flusher).Flush()
			<-r.Context().Done()
			return
		}
		_, _ = io.WriteString(w, "ok")
	}))
	defer up.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	c, err := New(up.URL, "synthetic-managed-token")
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.DoDownload(ctx, http.MethodGet, mustURL("/d/file"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if body := readAll(t, resp.Body); body != "ok" {
		t.Fatalf("body = %q", body)
	}
}

type downloadRoundTripFunc func(*http.Request) (*http.Response, error)

func (f downloadRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func TestDoDownloadConcurrentTokenReplacementAvoidsLogin(t *testing.T) {
	c, err := New("http://openlist.invalid", "synthetic-stale-token")
	if err != nil {
		t.Fatal(err)
	}
	c.SetCredentials("synthetic-admin", "synthetic-password")
	attempts := 0
	c.hc.Transport = downloadRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != "/d/file" {
			t.Fatalf("unexpected administrator request to %s", r.URL.Path)
		}
		attempts++
		status, body := http.StatusOK, "ok"
		if attempts == 1 {
			if r.Header.Get("Authorization") != "synthetic-stale-token" {
				t.Fatal("first request did not carry the stale token")
			}
			c.SetToken("synthetic-concurrent-token")
			status, body = http.StatusUnauthorized, ""
		} else if r.Header.Get("Authorization") != "synthetic-concurrent-token" {
			t.Fatal("retry did not use the concurrent replacement")
		}
		return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	})
	resp, err := c.DoDownload(context.Background(), http.MethodGet, mustURL("/d/file"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if readAll(t, resp.Body) != "ok" || attempts != 2 {
		t.Fatalf("expected a successful retry; attempts = %d", attempts)
	}
}

func TestDoDownloadHealFailureHidesSensitiveTransportError(t *testing.T) {
	c, err := New("http://openlist.invalid", "synthetic-stale-token")
	if err != nil {
		t.Fatal(err)
	}
	c.SetCredentials("synthetic-admin", "synthetic-password")
	const secret = "synthetic-private-signed-url"
	c.hc.Transport = downloadRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/api/auth/login" {
			return nil, errors.New(secret)
		}
		return &http.Response{StatusCode: http.StatusUnauthorized, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("")), Request: r}, nil
	})
	resp, err := c.DoDownload(context.Background(), http.MethodGet, mustURL("/d/file?sign="+secret), nil)
	if resp != nil || err == nil {
		t.Fatalf("expected a failed heal; response = %v, error = %v", resp, err)
	}
	if strings.Contains(err.Error(), secret) || err.Error() != "openlist download: upstream request failed" {
		t.Fatalf("error leaks transport details: %v", err)
	}
}
