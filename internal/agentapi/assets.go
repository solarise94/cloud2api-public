package agentapi

import (
	"fmt"
	"net/http"
	"strings"
)

// renderInstructions produces /custom/agent-instructions.txt for the request
// base URL: the BASIC surface only (discovery via /v1/me, canonical per-drive
// file operations, transfers, job polling, links), never containing a key.
// Advanced endpoints and the Gokapi compatibility layer stay unadvertised
// even though their routes remain available.
func (s *Server) renderInstructions(r *http.Request) string {
	return renderInstructionsText(s.baseURL(r), singleLine(s.policyText()))
}

func renderInstructionsText(base, policy string) string {
	b := &strings.Builder{}
	fmt.Fprintf(b, "cloud2api agent gateway — integration instructions\n")
	fmt.Fprintf(b, "==================================================\n\n")
	fmt.Fprintf(b, "Base URL: %s\n", base)
	fmt.Fprintf(b, "Authentication: `apikey` HTTP header carrying your API key (sk-...).\n")
	fmt.Fprintf(b, "  Authorization: Bearer <key> is also accepted. NEVER put the key in a URL,\n")
	fmt.Fprintf(b, "  query parameter, log line or repository.\n\n")

	b.WriteString("1. Discovery: GET /v1/me — always first\n")
	fmt.Fprintf(b, "   curl -H \"apikey: $C2A_KEY\" %s/v1/me\n", base)
	b.WriteString("   The response lists your drives (id, root chroot, per-drive permissions,\n")
	b.WriteString("   health), your key's default_drive, limits, trash settings and the storage\n")
	b.WriteString("   policy. Every file operation below is addressed PER DRIVE,\n")
	b.WriteString("   /v1/drives/<drive>/..., taking <drive> from /v1/me — use default_drive\n")
	b.WriteString("   when the task does not name a drive. Your effective permission on an\n")
	b.WriteString("   already-granted drive is the union of your global_perms and that drive's\n")
	b.WriteString("   `perms` entry; an operation you lack the effective permission for answers\n")
	b.WriteString("   403 forbidden and is final — do not retry it or work around it; report\n")
	b.WriteString("   instead.\n")
	b.WriteString("   Drive health vocabulary: healthy, needs_reauth (an administrator must\n")
	b.WriteString("   update the drive's login credentials in OpenList), error, disabled,\n")
	b.WriteString("   orphaned (the OpenList storage was deleted) and replacement_found (the\n")
	b.WriteString("   storage was replaced; access is blocked until an administrator confirms\n")
	b.WriteString("   the replacement in the cloud2api portal). Unhealthy drives answer 503\n")
	b.WriteString("   drive_unavailable on file operations.\n\n")

	b.WriteString("2. Listing and metadata (needs the `list` permission)\n")
	fmt.Fprintf(b, "   curl -H \"apikey: $C2A_KEY\" \"%s/v1/drives/<drive>/list?path=/reports\"\n", base)
	fmt.Fprintf(b, "   curl -H \"apikey: $C2A_KEY\" \"%s/v1/drives/<drive>/stat?path=/reports/q3.pdf\"\n", base)
	b.WriteString("   list is paged (page, per_page) and returns {items, total}; stat returns\n")
	b.WriteString("   one entry with size, modified time and the crc64/sha256 recorded at\n")
	b.WriteString("   upload time when known. Paths are logical paths inside your root on that\n")
	b.WriteString("   drive — they never mention mounts or drivers.\n\n")

	b.WriteString("3. Download (needs the `read` permission)\n")
	fmt.Fprintf(b, "   curl -H \"apikey: $C2A_KEY\" -o local.bin \\\n")
	fmt.Fprintf(b, "        \"%s/v1/drives/<drive>/files?path=/reports/q3.pdf\"\n", base)
	b.WriteString("   Range requests are supported. Responses may carry X-C2A-SHA256 /\n")
	b.WriteString("   X-C2A-CRC64 headers recorded at upload time.\n\n")

	b.WriteString("4. Upload with integrity verification (needs the `write` permission)\n")
	fmt.Fprintf(b, "   curl -H \"apikey: $C2A_KEY\" -T local.bin \\\n")
	fmt.Fprintf(b, "        \"%s/v1/drives/<drive>/files?path=/tmp/local.bin\"\n", base)
	b.WriteString("   Raw bodies must carry Content-Length (curl -T does). Multipart with a\n")
	b.WriteString("   `file` part also works. The response returns {path, size, crc64, sha256}:\n")
	b.WriteString("   - crc64 is CRC-64/XZ as a decimal string (check value of \"123456789\" is\n")
	b.WriteString("     11051210869376104954);\n")
	b.WriteString("   - compare size and crc64 (or sha256) against your local file to prove the\n")
	b.WriteString("     full chain worked; you may also send X-Expected-CRC64 / X-Expected-SHA256\n")
	b.WriteString("     so the server verifies and deletes the upload on mismatch (422).\n")
	b.WriteString("   Parent directories are created automatically. If the target exists use\n")
	b.WriteString("   on_conflict=overwrite or on_conflict=rename (default is 409 already_exists).\n")
	b.WriteString("   Upload uncertainty: if the connection drops before you read the response,\n")
	b.WriteString("   the target may or may not hold the complete new content. Do not assume\n")
	b.WriteString("   either way — stat the path and compare size and checksums before you\n")
	b.WriteString("   rely on the content.\n\n")

	b.WriteString("5. mkdir, move, delete (mkdir/move need effective `write`; delete needs\n")
	b.WriteString("   effective `delete` = global_perms union that drive's perms)\n")
	fmt.Fprintf(b, "   POST %s/v1/drives/<drive>/mkdir  {\"path\":\"/reports/2026\"}  (idempotent, 200 when it exists)\n", base)
	fmt.Fprintf(b, "   POST %s/v1/drives/<drive>/move   {\"from\":\"/a\",\"to\":\"/b\"}   (within one drive)\n", base)
	fmt.Fprintf(b, "   POST %s/v1/drives/<drive>/delete {\"paths\":[\"/tmp/scratch.txt\"]}\n", base)
	b.WriteString("   Many keys do not carry the delete permission; a 403 forbidden means this\n")
	b.WriteString("   key may not delete on that drive — report it, never try to work around\n")
	b.WriteString("   it, and do not treat deletion as implied by write access. Deleting a path\n")
	b.WriteString("   that is already gone answers 200 already_absent, so ensure-absent\n")
	b.WriteString("   operations are naturally idempotent. Deleted files go to the server-side\n")
	b.WriteString("   retention shown in /v1/me (when enabled) and can be restored while\n")
	b.WriteString("   retained — that is a safety net, not a license to delete anything the\n")
	b.WriteString("   user did not ask to delete.\n\n")

	b.WriteString("6. Dry runs\n")
	b.WriteString("   delete, move and transfers accept \"dry_run\": true and answer with\n")
	b.WriteString("   would_affect {files, dirs, bytes} plus the first 200 affected items,\n")
	b.WriteString("   changing nothing. Dry-run first whenever an operation would affect more\n")
	b.WriteString("   than 100 files or 1 GiB, then decide with the numbers in front of you.\n\n")

	b.WriteString("7. Cross-drive copy/move: transfers\n")
	fmt.Fprintf(b, "   curl -H \"apikey: $C2A_KEY\" -H 'Content-Type: application/json' \\\n")
	fmt.Fprintf(b, "        -d '{\"from\":{\"drive\":\"<src-drive>\",\"path\":\"/f.bin\"},\"to\":{\"drive\":\"<dst-drive>\",\"path\":\"/f.bin\"},\"mode\":\"copy\"}' \\\n")
	fmt.Fprintf(b, "        %s/v1/transfers          -> 202 {\"job\":{\"id\",...}}\n", base)
	b.WriteString("   mode is \"copy\" (default) or \"move\"; dry_run is supported. The 202 means\n")
	b.WriteString("   the job was accepted — the copy is NOT done yet; poll the job.\n\n")

	b.WriteString("8. Jobs: poll by id\n")
	fmt.Fprintf(b, "   curl -H \"apikey: $C2A_KEY\" \"%s/v1/jobs/{id}?wait=60\"\n", base)
	b.WriteString("   ?wait=60 long-polls: the call returns as soon as the job state changes\n")
	b.WriteString("   or after 60 seconds. Terminal states: succeeded, failed, canceled; a\n")
	b.WriteString("   failed job carries an error message. Keep the job id you received from\n")
	b.WriteString("   /v1/transfers and keep polling that known id until it reaches a terminal\n")
	b.WriteString("   state.\n\n")

	b.WriteString("9. Share links\n")
	fmt.Fprintf(b, "   curl -H \"apikey: $C2A_KEY\" -H 'Content-Type: application/json' \\\n")
	fmt.Fprintf(b, "        -d '{\"drive\":\"<drive>\",\"path\":\"/reports/q3.pdf\",\"expires_in\":3600}' \\\n")
	fmt.Fprintf(b, "        %s/v1/links\n", base)
	fmt.Fprintf(b, "   The response carries {url, expires_at}; url is a %s/l/<token> address\n", base)
	b.WriteString("   that works without an API key until it expires (max 7 days), afterwards\n")
	b.WriteString("   it answers 410. Revoking the key kills its links too.\n\n")

	b.WriteString("10. Retries and idempotency\n")
	b.WriteString("   Add an `Idempotency-Key` header (a fresh random value per logical\n")
	b.WriteString("   operation) to mutating calls (upload, mkdir, delete, move, transfers,\n")
	b.WriteString("   links). A repeat with the SAME key within 24 hours replays the stored\n")
	b.WriteString("   successful response (X-Idempotent-Replay: true) instead of repeating the\n")
	b.WriteString("   write. Failures are never cached and run again for real, so never assume\n")
	b.WriteString("   a failed or interrupted call \"did nothing\": check the outcome first\n")
	b.WriteString("   (stat the target and compare checksums, or poll the job), then retry\n")
	b.WriteString("   with the same key only if the operation still needs to happen.\n\n")

	b.WriteString("11. Errors\n")
	b.WriteString("   Every error is {\"error\":{code,message,retryable,retry_after?,hint,docs}}.\n")
	b.WriteString("   Stable codes: unauthorized, forbidden, drive_not_found, drive_unavailable,\n")
	b.WriteString("   path_invalid, path_not_found, already_exists, too_large, rate_limited,\n")
	b.WriteString("   checksum_mismatch, upstream_error (plus invalid_request for malformed\n")
	b.WriteString("   input). retryable=true means wait and retry; rate_limited carries a\n")
	b.WriteString("   Retry-After header. path_not_found carries up to 3 `suggestions` from\n")
	b.WriteString("   the parent directory; drive_unavailable carries a `hint` naming the\n")
	b.WriteString("   netdisk problem. Chroot escapes answer 400 path_invalid.\n\n")

	b.WriteString("12. Autonomy boundaries\n")
	b.WriteString("   You may do directly: read and list inside your key's roots; upload and\n")
	b.WriteString("   create directories; move and copy; delete files when the task requires\n")
	b.WriteString("   it and your key carries the effective delete permission (global_perms\n")
	b.WriteString("   union that drive's perms); create links; run transfers. Check\n")
	b.WriteString("   global_perms and the per-drive `perms` in /v1/me before acting.\n")
	b.WriteString("   Anything outside your grants (extra drives, wider roots, delete or other\n")
	b.WriteString("   additional permissions, higher limits, updated netdisk credentials)\n")
	b.WriteString("   needs the user to ask an administrator — there is no agent-side approval\n")
	b.WriteString("   endpoint; report the limitation instead.\n")
	b.WriteString("   Never do: delete files you did not create unless the user asked; put the\n")
	b.WriteString("   API key into URLs, logs or code repositories.\n\n")

	fmt.Fprintf(b, "13. Storage policy\n   %s\n\n", policy)

	b.WriteString("14. Tooling\n")
	fmt.Fprintf(b, "   OpenAPI spec (basic surface): %s/apidocumentation/openapi.json\n", base)
	fmt.Fprintf(b, "   Python CLI helper: %s/custom/c2a.py  (stdlib only; exports C2A_URL/C2A_KEY;\n", base)
	b.WriteString("                      me ls stat put get rm mv cp mkdir link jobs get;\n")
	b.WriteString("                      put/get verify SHA-256; rm/mv/cp take --dry-run;\n")
	b.WriteString("                      jobs get takes --wait; --drive targets a drive,\n")
	b.WriteString("                      otherwise the key default_drive from me is used)\n")
	fmt.Fprintf(b, "   Skill file:        %s/custom/SKILL.md\n", base)
	b.WriteString("\nIntegration flow: call GET /v1/me, then work directly with the per-drive\n")
	b.WriteString("endpoints above. A connect-time write test is not required and not\n")
	b.WriteString("expected. Only when the user explicitly asks for a write diagnostic:\n")
	b.WriteString("upload a small file of your own, verify size + CRC64 from the response,\n")
	b.WriteString("and delete it again if your key carries the effective delete permission.\n")
	b.WriteString("Do not touch existing files beyond what the task requires; when the network\n")
	b.WriteString("blocks you, report it truthfully instead of retrying forever.\n")
	return b.String()
}

func singleLine(s string) string {
	return strings.Join(strings.Fields(strings.ReplaceAll(s, "\n", " ")), " ")
}

// renderGokapiInstructions produces /custom/gokapi-agent-instructions.txt: the
// Gokapi relay onboarding text rendered with this gateway's base URL. This is
// an explicit compatibility asset for existing Gokapi-style consumers; it is
// deliberately NOT linked from the default basic onboarding (instructions,
// SKILL.md, docs HTML). It contains no key; per-key limits are discoverable
// through /v1/me.
func (s *Server) renderGokapiInstructions(r *http.Request) string {
	return renderGokapiInstructionsText(s.baseURL(r))
}

func renderGokapiInstructionsText(base string) string {
	b := &strings.Builder{}
	fmt.Fprintf(b, "Gokapi Agent File Relay — connection and usage\n\n")
	fmt.Fprintf(b, "This relay is already deployed and configured (cloud2api Gokapi compatibility\n")
	fmt.Fprintf(b, "layer). Use the base URL and API key supplied in the user's prompt.\n\n")
	fmt.Fprintf(b, "Base URL: %s\n", base)
	fmt.Fprintf(b, "Authentication: HTTP header `apikey: <API_KEY>` (Bearer is also accepted).\n")
	b.WriteString("This compatibility layer is documented for existing Gokapi-style consumers\n")
	b.WriteString("in the cloud2api project documentation; it is not part of the default agent\n")
	b.WriteString("surface and is not included in the default OpenAPI document.\n\n")

	b.WriteString("1. Connection\n")
	fmt.Fprintf(b, "  curl --fail-with-body --silent --show-error \\\n")
	fmt.Fprintf(b, "    \"$GOKAPI_URL/api/info/version\" -H \"apikey: $GOKAPI_TOKEN\"\n\n")

	b.WriteString("2. Upload files\n")
	fmt.Fprintf(b, "  curl --fail-with-body --silent --show-error \\\n")
	fmt.Fprintf(b, "    \"$GOKAPI_URL/api/files/add\" \\\n")
	fmt.Fprintf(b, "    -H \"apikey: $GOKAPI_TOKEN\" -H \"accept: application/json\" \\\n")
	fmt.Fprintf(b, "    -F \"file=@/absolute/path/to/file\" \\\n")
	fmt.Fprintf(b, "    -F 'allowedDownloads=0' -F 'expiryDays=7'\n")
	b.WriteString("Do not manually set multipart Content-Type. Stream files from disk, provide\n")
	b.WriteString("Content-Length (requests with unknown body length answer 411). Set\n")
	b.WriteString("allowedDownloads=0 and expiryDays=7 explicitly on every upload. The server\n")
	b.WriteString("enforces a maximum seven-day lifetime even if expiryDays=0 or a larger value\n")
	b.WriteString("is sent. Password-protected uploads are rejected (400).\n\n")
	b.WriteString("Successful JSON has Result=\"OK\" and FileInfo, including Id, Name, SizeBytes\n")
	b.WriteString("and CRC64: the CRC-64/XZ of the stored object as an unsigned 64-bit DECIMAL\n")
	b.WriteString("STRING (avoid JavaScript Number precision loss). Verify the upload by\n")
	b.WriteString("comparing HTTP success, Result, SizeBytes with the local file size and\n")
	b.WriteString("FileInfo.CRC64 with the locally computed CRC-64/XZ:\n")
	b.WriteString("  import crcmod\n")
	b.WriteString("  checksum = crcmod.Crc(0x142F0E1EBA9EA3693, initCrc=0,\n")
	b.WriteString("                       xorOut=0xffffffffffffffff, rev=True)\n")
	b.WriteString("  with open(file_path, \"rb\") as source:\n")
	b.WriteString("      for chunk in iter(lambda: source.read(1024*1024), b\"\"):\n")
	b.WriteString("          checksum.update(chunk)\n")
	b.WriteString("  assert str(checksum.crcValue) == response[\"FileInfo\"][\"CRC64\"]\n")
	b.WriteString("The reference bytes b\"123456789\" must yield \"11051210869376104954\".\n")
	b.WriteString("Do not download a file solely to verify upload success.\n\n")

	b.WriteString("2b. Upload receipts (what actually happened)\n")
	b.WriteString("Every upload response carries additive fields UploadId, Status and Message\n")
	b.WriteString("(and the X-Upload-ID response header). Send your own random id with the\n")
	b.WriteString("X-Upload-ID request header; otherwise the server mints one. An upload id\n")
	b.WriteString("is the immutable identity of ONE attempt: reusing it for another upload\n")
	b.WriteString("answers 409 — always start a retry with a FRESH id, and query the old id\n")
	b.WriteString("(unchanged) to learn what the previous attempt did. Chunk staging may\n")
	b.WriteString("repeat the same id (/api/chunk/add); completion reuses it exactly once.\n")
	b.WriteString("Query the durable outcome with the SAME API key:\n")
	fmt.Fprintf(b, "  curl --fail-with-body \"$GOKAPI_URL/api/files/status/$UPLOAD_ID\" \\\n")
	fmt.Fprintf(b, "    -H \"apikey: $GOKAPI_TOKEN\"\n")
	b.WriteString("Status values: uploading (still receiving/submitting — never claim done),\n")
	b.WriteString("completed (upstream upload confirmed AND metadata persisted; FileInfo with\n")
	b.WriteString("SizeBytes/CRC64 is included), failed (explicit validation/quota/upstream\n")
	b.WriteString("error in Message), interrupted (connection dropped or the gateway stopped;\n")
	b.WriteString("the remote may hold a partial or complete object — verify, do not assume\n")
	b.WriteString("rollback). Unknown or foreign ids answer 404. Chunk staging acks\n")
	b.WriteString("(/api/chunk/add) are NOT completion; a nonblocking /api/chunk/complete\n")
	b.WriteString("answers Status=uploading — poll the status endpoint for the terminal\n")
	b.WriteString("state before reporting success. Reconnect after a disconnect and query the\n")
	b.WriteString("known upload id; no result means the outcome is unconfirmed, not success.\n\n")

	b.WriteString("3. Download files\n")
	b.WriteString("Use the returned FileInfo.UrlHotlink for a raw download, or construct:\n")
	fmt.Fprintf(b, "  %s/downloadFile?id=<FILE_ID>\n", base)
	fmt.Fprintf(b, "Example:\n")
	fmt.Fprintf(b, "  curl --fail --location \"$GOKAPI_URL/downloadFile?id=$FILE_ID\" -o output.bin\n")
	b.WriteString("These generated links can be downloaded without the API key. The file ID\n")
	b.WriteString("itself grants access; share it only with intended recipients. Never put the\n")
	b.WriteString("upload API key into a URL. UrlDownload is an HTML share page (/d?id=).\n\n")

	b.WriteString("4. List and delete\n")
	fmt.Fprintf(b, "  curl --fail-with-body \"$GOKAPI_URL/api/files/list\" -H \"apikey: $GOKAPI_TOKEN\"\n")
	b.WriteString("The result is an array, or null when empty. Metadata of one file:\n")
	fmt.Fprintf(b, "  curl --fail-with-body \"$GOKAPI_URL/api/files/list/$FILE_ID\" -H \"apikey: $GOKAPI_TOKEN\"\n")
	b.WriteString("Delete only a file the user asked you to delete, or your own temporary test\n")
	b.WriteString("file. Immediate deletion, or a cancellable delayed one:\n")
	fmt.Fprintf(b, "  curl --fail-with-body -X DELETE \"$GOKAPI_URL/api/files/delete\" \\\n")
	fmt.Fprintf(b, "    -H \"apikey: $GOKAPI_TOKEN\" -H \"id: $FILE_ID\"\n")
	fmt.Fprintf(b, "  curl --fail-with-body -X DELETE \"$GOKAPI_URL/api/files/delete\" \\\n")
	fmt.Fprintf(b, "    -H \"apikey: $GOKAPI_TOKEN\" -H \"id: $FILE_ID\" -H \"delay: 120\"\n")
	fmt.Fprintf(b, "  curl --fail-with-body -X POST \"$GOKAPI_URL/api/files/restore\" \\\n")
	fmt.Fprintf(b, "    -H \"apikey: $GOKAPI_TOKEN\" -H \"id: $FILE_ID\"\n")
	b.WriteString("User, API key, file request and log endpoints are not supported (403).\n\n")

	b.WriteString("5. Storage, capacity and retention\n")
	b.WriteString("Every upload and download passes through the gateway host; file contents are\n")
	b.WriteString("stored on the netdisk drive behind it. Default policy per API key: 200 GiB\n")
	b.WriteString("capacity with oldest-file eviction, files expire no later than seven days\n")
	b.WriteString("after upload, cleanup runs every five minutes and deletes permanently.\n")
	b.WriteString("Your key's effective limits (drive, quota, max age) are shown in /v1/me\n")
	b.WriteString("under \"relay\". The configured maximum file size is 8 GiB. For large files\n")
	b.WriteString("use chunk uploads: POST /api/chunk/add (multipart file, uuid >= 10 chars,\n")
	b.WriteString("filesize, offset; chunks are normally 45 MiB, minimum 5 MiB except the last)\n")
	b.WriteString("and finalise with POST /api/chunk/complete (headers uuid, filename,\n")
	b.WriteString("filesize, contenttype, allowedDownloads, expiryDays, nonblocking). Keep one\n")
	b.WriteString("uuid and the same total file size throughout an upload. Use blocking\n")
	b.WriteString("completion (nonblocking=false) to obtain the final FileInfo and CRC64; an\n")
	b.WriteString("asynchronous acknowledgement is not proof of a completed upload — poll\n")
	b.WriteString("GET /api/files/status/{UploadId} (same API key) for the terminal state.\n")
	b.WriteString("Abandoned upload chunks are cleaned after one day. Capacity/expiry\n")
	b.WriteString("deletion makes old links stop working; keep durable originals elsewhere.\n\n")

	b.WriteString("6. Integration workflow\n")
	b.WriteString("If you already have Gokapi relay instructions, swap the Base URL and API key\nand keep working exactly as before. Otherwise adapt the existing project's\nlanguage/client. Run one small verified test upload, compare local\nsize/CRC64 with the server response, and delete only that test file.\n")
	b.WriteString("Download only when the task actually needs the file. Preserve any existing\n")
	b.WriteString("uploaded files. If a tool/network restriction prevents access, report the\n")
	b.WriteString("actual error without claiming integration succeeded.\n")
	return b.String()
}

// renderSKILL produces /custom/SKILL.md: the BASIC surface only, matching
// renderInstructionsText (no advanced endpoints, no compatibility links).
func renderSKILL(base, policy string) string {
	p := singleLine(policy)
	var b strings.Builder
	b.WriteString("# cloud2api netdisk access\n\n")
	b.WriteString("Use this skill whenever a task needs to read or write files on the user's\n")
	b.WriteString("netdisk through the cloud2api gateway.\n\n")
	b.WriteString("## Setup\n\n")
	b.WriteString("Environment: `C2A_URL` (gateway base URL) and `C2A_KEY` (API key).\n")
	b.WriteString(fmt.Sprintf("Fetch full instructions from `%s/custom/agent-instructions.txt`\n", base))
	b.WriteString(fmt.Sprintf("and the basic surface spec from `%s/apidocumentation/openapi.json`.\n\n", base))
	b.WriteString("## Core rules\n\n")
	b.WriteString("- Send the key in the `apikey` header; never in URLs, logs or repos.\n")
	b.WriteString("- First call `GET /v1/me`; it lists the drives (id, root chroot, per-drive\n")
	b.WriteString("  perms, health), global_perms, the key's default_drive, limits and dynamic\n")
	b.WriteString("  `next_steps` (unhealthy drives, expiring retained deletions). Drive health\n")
	b.WriteString("  values: healthy, needs_reauth (an administrator must update the\n")
	b.WriteString("  credentials in OpenList), error, disabled, orphaned, replacement_found\n")
	b.WriteString("  (access blocked until an administrator confirms the replacement storage).\n")
	b.WriteString("- Address file operations per drive, exactly as discovered in /v1/me:\n")
	b.WriteString("  `/v1/drives/<drive>/list|stat|files|mkdir|delete|move`. Use the\n")
	b.WriteString("  default_drive when the task does not name a drive. Use logical paths\n")
	b.WriteString("  inside your root; paths never mention mounts or drivers.\n")
	b.WriteString("- After uploading, verify `size` and `crc64` (CRC-64/XZ decimal; the check\n")
	b.WriteString("  value of \"123456789\" is 11051210869376104954) against your local file, or\n")
	b.WriteString("  send X-Expected-CRC64 / X-Expected-SHA256 so the server verifies and\n")
	b.WriteString("  deletes mismatched uploads (422). If a connection drops mid-upload, the\n")
	b.WriteString("  target may or may not hold the new content: stat it and compare before\n")
	b.WriteString("  continuing.\n")
	b.WriteString("- Deleting requires the effective `delete` permission — the union of\n")
	b.WriteString("  global_perms and that drive's `perms` in /v1/me; many keys do not carry\n")
	b.WriteString("  it and a 403 forbidden is final — report, never work around, and never\n")
	b.WriteString("  treat deletion as implied by write access. Dry-run bulk work first:\n")
	b.WriteString("  `dry_run: true` on delete/move/transfers answers would_affect\n")
	b.WriteString("  {files, dirs, bytes}; anything over 100 files or 1 GiB must be dry-run\n")
	b.WriteString("  first. Never delete files you did not create unless the user asked.\n")
	b.WriteString("- Cross-drive copy/move: `POST /v1/transfers` answers 202 {job}; keep the\n")
	b.WriteString("  job id and poll `GET /v1/jobs/{id}?wait=60` (long-poll, returns on state\n")
	b.WriteString("  change) until it reaches a terminal state.\n")
	b.WriteString("- Mutating calls take an `Idempotency-Key`: a repeat with the same key\n")
	b.WriteString("  within 24h replays the stored successful response. Failures are never\n")
	b.WriteString("  cached — after a failure or a dropped connection verify the outcome\n")
	b.WriteString("  first, then retry only if the operation still needs to happen.\n")
	b.WriteString("- `path_not_found` errors carry up to 3 `suggestions`; `drive_unavailable`\n")
	b.WriteString("  carries a `hint` naming the netdisk problem (an administrator fixes drives\n")
	b.WriteString("  in OpenList; there is no agent-side approval endpoint).\n")
	b.WriteString("- Do not delete files you did not create unless the user asked.\n")
	b.WriteString(fmt.Sprintf("- Policy: %s\n\n", p))
	b.WriteString("## Helper CLI\n\n")
	b.WriteString(fmt.Sprintf("`%s/custom/c2a.py` (Python 3 stdlib only): `python3 c2a.py me|ls|stat|put|get|rm|mv|cp|mkdir|link|jobs`.\n", base))
	b.WriteString("`put` and `get` verify SHA-256 automatically and exit non-zero with the\n")
	b.WriteString("server's error JSON on failure. `rm`, `mv` and `cp` take `--dry-run`;\n")
	b.WriteString("`jobs get ID --wait 60` long-polls; `cp A B --from-drive D --to-drive D2`\n")
	b.WriteString("runs a transfer job and waits for it. Pass `--drive D` to target a specific\n")
	b.WriteString("drive; without it the helper uses the key's default_drive from `me`.\n")
	return b.String()
}

// apiDocsIndexHTML produces /apidocumentation: a short index of the BASIC
// endpoint surface plus the asset links. Advanced and compatibility surfaces
// are intentionally not listed or linked here.
func apiDocsIndexHTML(base string) string {
	rows := []string{
		"GET /v1/me — this key's drives, permissions, limits, policy",
		"GET /v1/drives/{drive}/list?path=... — list a directory (paged)",
		"GET /v1/drives/{drive}/stat?path=... — one entry's metadata",
		"GET /v1/drives/{drive}/files?path=... — download (Range supported)",
		"PUT /v1/drives/{drive}/files?path=... — upload; verify crc64/sha256",
		"POST /v1/drives/{drive}/mkdir — create a directory (idempotent)",
		"POST /v1/drives/{drive}/move — move or rename within the drive",
		"POST /v1/drives/{drive}/delete — delete paths (delete permission; dry_run supported)",
		"POST /v1/transfers — cross-drive copy/move as a job (202)",
		"GET /v1/jobs/{id}?wait=N — long-poll one job by id",
		"POST /v1/links — create a limited-time direct link",
		"GET /l/{token} — download via a link (no API key)",
	}
	var list strings.Builder
	for _, r := range rows {
		list.WriteString("<li><code>" + r + "</code></li>\n")
	}
	return `<!doctype html><html><head><meta charset="utf-8"><title>cloud2api API documentation</title></head>
<body style="font-family: system-ui, sans-serif; max-width: 40rem; margin: 3rem auto; line-height: 1.5">
<h1>cloud2api agent API</h1>
<p>The machine-readable OpenAPI document (basic surface) is at
<a href="` + base + `/apidocumentation/openapi.json">/apidocumentation/openapi.json</a>.</p>
<h2>Basic endpoints</h2>
<p>All file operations are addressed per drive: <code>/v1/drives/&lt;drive&gt;/...</code>
with the drive id from <code>GET /v1/me</code> (use the key's default_drive when the task
does not name one).</p>
<ul>
` + list.String() + `</ul>
<p>Agent onboarding instructions: <a href="` + base + `/custom/agent-instructions.txt">/custom/agent-instructions.txt</a><br>
Python helper: <a href="` + base + `/custom/c2a.py">/custom/c2a.py</a><br>
Skill file: <a href="` + base + `/custom/SKILL.md">/custom/SKILL.md</a></p>
</body></html>`
}
