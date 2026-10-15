package goapihop

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/OpenListTeam/OpenList/v4/pkg/goapiclient"
	"github.com/gin-gonic/gin"
)

func TestMiddleware(t *testing.T) {
	gin.SetMode(gin.TestMode)
	run := func(header []string) (status int, ctxHop int) {
		engine := gin.New()
		engine.Use(Middleware())
		engine.Any("/probe", func(c *gin.Context) {
			ctxHop, _ = goapiclient.HopFromContext(c.Request.Context())
			c.Status(http.StatusOK)
		})
		req := httptest.NewRequest(http.MethodGet, "/probe", nil)
		for _, h := range header {
			req.Header.Add(goapiclient.HopHeader, h)
		}
		w := httptest.NewRecorder()
		engine.ServeHTTP(w, req)
		return w.Code, ctxHop
	}

	if status, hop := run(nil); status != http.StatusOK || hop != 0 {
		t.Fatalf("no header: status=%d hop=%d, want 200/0", status, hop)
	}
	if status, hop := run([]string{"2"}); status != http.StatusOK || hop != 2 {
		t.Fatalf("hop 2: status=%d hop=%d, want 200/2", status, hop)
	}
	if status, hop := run([]string{"0"}); status != http.StatusOK || hop != 0 {
		t.Fatalf("hop 0: status=%d hop=%d, want 200/0", status, hop)
	}
	if status, _ := run([]string{strconv.Itoa(goapiclient.MaxHops)}); status != http.StatusOK {
		t.Fatalf("hop at the cap must pass, got %d", status)
	}
	if status, _ := run([]string{"abc"}); status != http.StatusBadRequest {
		t.Fatalf("malformed hop must 400, got %d", status)
	}
	if status, _ := run([]string{"-3"}); status != http.StatusBadRequest {
		t.Fatalf("negative hop must 400, got %d", status)
	}
	if status, _ := run([]string{"1", "2"}); status != http.StatusBadRequest {
		t.Fatalf("duplicate hop headers must 400, got %d", status)
	}
	if status, _ := run([]string{strconv.Itoa(goapiclient.MaxHops + 1)}); status != http.StatusForbidden {
		t.Fatalf("hop above the cap must 403, got %d", status)
	}
}
