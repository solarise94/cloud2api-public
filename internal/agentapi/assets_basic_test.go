package agentapi

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// The default onboarding assets (instructions, SKILL.md, docs HTML, served
// helper) must present the BASIC surface only. The routes themselves stay
// available; they are simply not advertised to agents, who read everything
// they download.
const (
	basicTestBase   = "http://example.com"
	basicTestPolicy = "max upload 8 GiB per file; /.c2a-trash trash retention 7 days by default; other paths are durable netdisk storage — do not delete files you did not create. Traffic passes through the gateway host."
)

// advancedAssetPaths are endpoint path fragments that must not appear in the
// basic assets: advanced endpoints, the Gokapi compatibility layer, the admin
// portal and the MCP surface.
var advancedAssetPaths = []string{
	"/v1/selftest", "/v1/tree", "/v1/search", "/v1/batch", "/v1/fetch",
	"/v1/keys", "/v1/activity", "/v1/trash",
	"/api/", "/custom/gokapi", "/admin/", "/mcp",
}

// aliasDrivePaths are the default-drive alias URLs; the basic assets document
// only the canonical per-drive form /v1/drives/<drive>/....
var aliasDrivePaths = []string{
	"/v1/list", "/v1/stat", "/v1/files", "/v1/mkdir", "/v1/delete", "/v1/move",
}

// normWS collapses whitespace so phrase assertions survive text re-wrapping.
func normWS(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func assertBasicOnly(t *testing.T, name, body string) {
	t.Helper()
	for _, bad := range advancedAssetPaths {
		if strings.Contains(body, bad) {
			t.Errorf("%s advertises advanced/compat endpoint %q", name, bad)
		}
	}
	for _, alias := range aliasDrivePaths {
		if strings.Contains(body, alias) {
			t.Errorf("%s documents default-drive alias URL %q; use /v1/drives/<drive>/...", name, alias)
		}
	}
	for _, jobsList := range []string{"/v1/jobs?", "/v1/jobs/cancel", "jobs ls", "jobs cancel"} {
		if strings.Contains(body, jobsList) {
			t.Errorf("%s suggests job list/cancel via %q (only jobs/{id} polling is basic)", name, jobsList)
		}
	}
}

func requireAll(t *testing.T, name, body string, want []string) {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(body, w) {
			t.Errorf("%s lacks %q", name, w)
		}
	}
}

func TestInstructionsBasicSurface(t *testing.T) {
	body := normWS(renderInstructionsText(basicTestBase, basicTestPolicy))
	assertBasicOnly(t, "agent-instructions.txt", body)
	requireAll(t, "agent-instructions.txt", body, []string{
		"Base URL: " + basicTestBase,
		"apikey",
		"/v1/me",
		"default_drive",
		"/v1/drives/<drive>/list",
		"/v1/drives/<drive>/stat",
		"/v1/drives/<drive>/files?path=",
		"/v1/drives/<drive>/mkdir",
		"/v1/drives/<drive>/move",
		"/v1/drives/<drive>/delete",
		"/v1/transfers",
		"/v1/jobs/{id}?wait=60",
		"/l/<token>",
		// Safety and verification content that must survive the trim.
		"11051210869376104954",
		"X-Expected-CRC64",
		"Idempotency-Key",
		"dry_run",
		"would_affect",
		"403 forbidden",
		"may or may not", // upload outcome uncertainty
		"union of your global_perms and that drive's `perms` entry",
		"do not treat deletion as implied by write access",
		"keep polling that known id",
		"A connect-time write test is not required and not expected",
		"Only when the user explicitly asks for a write diagnostic",
		"Autonomy boundaries",
		// The dynamic policy text stays, truthfully.
		basicTestPolicy,
	})
	// Retry guidance must not promise blind safe retries of failures.
	if strings.Contains(body, "blindly") {
		t.Error("instructions promise blind retries; they must teach verify-then-retry")
	}
	// The upload-uncertainty text must not nudge towards a blind overwrite.
	if strings.Contains(body, "on_conflict=overwrite) before") || strings.Contains(body, "or upload again with") {
		t.Error("upload uncertainty text suggests a blind overwrite after an unknown result")
	}
}

func TestSKILLBasicSurface(t *testing.T) {
	body := normWS(renderSKILL(basicTestBase, basicTestPolicy))
	assertBasicOnly(t, "SKILL.md", body)
	requireAll(t, "SKILL.md", body, []string{
		"GET /v1/me",
		"/v1/drives/<drive>/",
		"11051210869376104954",
		"Idempotency-Key",
		"dry_run",
		"effective `delete` permission — the union of global_perms and that drive's `perms`",
		"/v1/transfers",
		"poll `GET /v1/jobs/{id}?wait=60` (long-poll, returns on state change) until it reaches a terminal state",
		"custom/c2a.py",
		"me|ls|stat|put|get|rm|mv|cp|mkdir|link|jobs",
		"Never delete files you did not create unless the user asked",
		basicTestPolicy,
	})
	if strings.Contains(body, "blindly") {
		t.Error("SKILL.md promises blind retries")
	}
	if strings.Contains(body, "the only way") {
		t.Error("SKILL.md must not claim jobs-by-id is the only way to observe a job")
	}
}

func TestAPIDocsHTMLBasicSurface(t *testing.T) {
	body := apiDocsIndexHTML(basicTestBase)
	assertBasicOnly(t, "apidocumentation HTML", body)
	requireAll(t, "apidocumentation HTML", body, []string{
		"/v1/me",
		"GET /v1/drives/{drive}/files",
		"PUT /v1/drives/{drive}/files",
		"/v1/transfers",
		"/v1/jobs/{id}?wait=N",
		"/l/{token}",
		"apidocumentation/openapi.json",
		"custom/agent-instructions.txt",
		"custom/c2a.py",
		"custom/SKILL.md",
	})
}

// The Gokapi onboarding asset remains available as an explicit compatibility
// surface for existing consumers, but it must not point at the default
// OpenAPI document any more (the default spec covers the basic surface only)
// and must not be linked from the basic assets (assertBasicOnly above).
func TestGokapiInstructionsStayCompatOnly(t *testing.T) {
	body := renderGokapiInstructionsText(basicTestBase)
	requireAll(t, "gokapi-agent-instructions.txt", body, []string{
		"Base URL: " + basicTestBase,
		"/api/files/add",
		"allowedDownloads=0",
		"expiryDays=7",
		"11051210869376104954",
		"/downloadFile?id=",
		"swap the Base URL and API key",
	})
	if strings.Contains(body, "apidocumentation") {
		t.Error("gokapi instructions still reference the default OpenAPI document")
	}
}

// TestC2APySourceBasicSurface pins the served helper to the basic command set
// and to basic URLs only: agents read the source they download, so hidden
// help would not hide anything.
func TestC2APySourceBasicSurface(t *testing.T) {
	src := c2aPySource
	assertBasicOnly(t, "c2a.py source", src)

	re := regexp.MustCompile(`sub\.add_parser\("([a-z]+)"\)`)
	var cmds []string
	for _, m := range re.FindAllStringSubmatch(src, -1) {
		cmds = append(cmds, m[1])
	}
	want := []string{"me", "ls", "stat", "put", "get", "rm", "mv", "cp", "mkdir", "link", "jobs"}
	if strings.Join(cmds, ",") != strings.Join(want, ",") {
		t.Errorf("c2a.py commands = %v, want %v", cmds, want)
	}

	for _, bad := range []string{"advanced", "selftest", "trash", `"cancel"`, "delete HTTP"} {
		if strings.Contains(strings.ToLower(src), bad) {
			t.Errorf("c2a.py source contains %q", bad)
		}
	}
	requireAll(t, "c2a.py source", normWS(src), []string{
		"C2A_URL",
		"C2A_KEY",
		"X-Expected-SHA256",
		"default_drive",
		"--drive",
		`urllib.parse.quote(drive, safe="")`,
		`"/v1/me"`,
		`"/v1/drives/%s/%s"`,
		`"/v1/transfers"`,
		`"/v1/links"`,
		`"/v1/jobs/%s`,
		`choices=["get"]`,
		// cp exits non-zero unless the transfer job succeeded.
		`job.get("state") != "succeeded"`,
		// Permission wording: effective permission = global union drive perms.
		normWS("what is effective on an already-granted drive is the union of the key's global_perms and that drive's perms"),
	})
}

// The developer helper keeps the full command surface; it is repo-only and
// not served.
func TestToolsFullHelperRetained(t *testing.T) {
	raw, err := os.ReadFile("../../tools/c2a-full.py")
	if err != nil {
		t.Fatalf("read tools/c2a-full.py: %v", err)
	}
	src := string(raw)
	requireAll(t, "tools/c2a-full.py", src, []string{
		"C2A_URL",
		"C2A_KEY",
		// Advanced commands stay usable via the repo tool.
		"selftest", "tree", "search", "keys", "activity", "trash",
		`"/v1/selftest"`, `"/v1/tree?"`, `"/v1/search?"`, `"/v1/keys"`,
		`"/v1/activity"`, `"/v1/trash"`,
		`choices=["ls", "get", "cancel"]`,
		// It keeps the historical default-drive alias form.
		`"/v1/%s"`,
	})
	if !strings.Contains(src, "developer") {
		t.Error("tools/c2a-full.py doc header should name the developer helper")
	}
}
