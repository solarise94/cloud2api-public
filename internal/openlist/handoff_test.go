package openlist

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

const fakeHandoffCode = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

func TestStartConnectHandoff(t *testing.T) {
	var calls atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != http.MethodPost || r.URL.Path != "/api/admin/connect/handoff/start" || r.Header.Get("Authorization") != "synthetic-management-token" {
			t.Errorf("unexpected handoff request method, path or authentication")
		}
		if r.URL.RawQuery != "" || r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("handoff request must use JSON and no query credentials")
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"code":200,"data":{"code":%q,"expires_in":60}}`, fakeHandoffCode)
	}))
	defer ts.Close()
	client, err := New(ts.URL, "synthetic-management-token")
	if err != nil {
		t.Fatal(err)
	}
	ticket, err := client.StartConnectHandoff(context.Background())
	if err != nil || ticket.Code != fakeHandoffCode || ticket.ExpiresIn != 60 || calls.Load() != 1 {
		t.Fatalf("handoff was not minted exactly once: ticket=%+v err=%v calls=%d", ticket, err, calls.Load())
	}
}

func TestStartConnectHandoffRejectsUnconfirmedResponses(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
		ct         string
		absent     bool
	}{
		{"SPA", "<html>OpenList</html>", 200, "text/html", true},
		{"HTTP404", "{}", 404, "application/json", true},
		{"envelope404", `{"code":404}`, 200, "application/json", true},
		{"redirect", "", 302, "application/json", false},
		{"HTTP500", fmt.Sprintf(`{"code":200,"data":{"code":%q,"expires_in":60}}`, fakeHandoffCode), 500, "application/json", false},
		{"missingCode", fmt.Sprintf(`{"data":{"code":%q,"expires_in":60}}`, fakeHandoffCode), 200, "application/json", false},
		{"zeroCode", `{"code":0,"data":{}}`, 200, "application/json", false},
		{"badJSON", `{`, 200, "application/json", false},
		{"trailingJSON", `{"code":200} {}`, 200, "application/json", false},
		{"oversize", strings.Repeat(" ", 65537), 200, "application/json", false},
		{"wrongCT", `{"code":200}`, 200, "text/plain", false},
		{"badTicket", `{"code":200,"data":{"code":"../unsafe","expires_in":60}}`, 200, "application/json", false},
		{"shortTicket", `{"code":200,"data":{"code":"AAA","expires_in":60}}`, 200, "application/json", false},
		{"expired", fmt.Sprintf(`{"code":200,"data":{"code":%q,"expires_in":0}}`, fakeHandoffCode), 200, "application/json", false},
		{"overlongTTL", fmt.Sprintf(`{"code":200,"data":{"code":%q,"expires_in":61}}`, fakeHandoffCode), 200, "application/json", false},
		{"secretError", `{"code":500,"message":"synthetic-management-token password apikey"}`, 200, "application/json", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, calls := newConnectClient(t, tc.status, tc.ct, tc.body)
			ticket, err := client.StartConnectHandoff(context.Background())
			if err == nil || ticket.Code != "" || calls.Load() != 1 {
				t.Fatalf("unconfirmed handoff accepted/retried: %+v, %v, calls=%d", ticket, err, calls.Load())
			}
			if errors.Is(err, ErrConnectUnavailable) != tc.absent {
				t.Errorf("unavailable classification = %v, want %v", err, tc.absent)
			}
			if strings.Contains(err.Error(), "synthetic-management-token") || strings.Contains(err.Error(), "password apikey") {
				t.Error("upstream secret/error text was echoed")
			}
		})
	}
}

func TestConnectHandoffCapabilityIsExplicit(t *testing.T) {
	for _, flag := range []bool{false, true} {
		client, _ := newConnectClient(t, 200, "application/json", fmt.Sprintf(`{"code":200,"data":{"module":{"version":"999","handoff":%t},"platforms":[]}}`, flag))
		info, err := client.ConnectInfo(context.Background())
		if err != nil || info.Handoff != flag {
			t.Fatalf("capability must not be inferred from version: %+v %v", info, err)
		}
	}
}
