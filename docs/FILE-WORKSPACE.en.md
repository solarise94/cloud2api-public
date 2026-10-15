[简体中文](FILE-WORKSPACE.md) | [English](FILE-WORKSPACE.en.md)

# My Files: a human-facing drive workbench

## Entry points and usage

The instance root `/` is a public navigation page offering only "File workspace" and "Administration portal" entries; it accepts no credentials. The file workbench is served together with the enabled admin portal and lives at `/files/`: it lists the logged-in workspaces still valid for the current browser and accepts an API Key **issued by this instance** to add a workspace; with exactly one valid workspace it is restored directly. Each workspace has a fixed entry `/files/w/<id>/`, where `id` is a randomly generated, non-secret identifier; the entry can be bookmarked, but the URL is not a credential — a link alone cannot access files, and other browsers must sign in with the corresponding Key. The admin portal sidebar and the Key creation, rotation, and usage dialogs also provide entry points; `/admin/` continues to be used for storage onboarding, authorization, and instance management.

1. Sign in with an API Key **issued by this instance**. Drive account passwords, the OpenList password, and the upstream GoAPI Key are not entered here. After a successful login the browser keeps an opaque HttpOnly session cookie (a fixed 30 days per workspace), and the raw Key is no longer retained in page memory; refreshing, closing and reopening the browser, and gateway restarts all continue from the favorited workspace. "Sign out" ends only the current workspace; other workspaces and the administrator session are unaffected.
2. If this Key is authorized for only one drive, logging in takes you directly to that drive's authorized root directory; if it is authorized for multiple drives, "My Drives" is shown first for selection, and the default drive is only marked as such. With no drive authorization, an empty state is shown, and authorization is never guessed at or widened; if a single drive lacks the list permission, it is still clearly stated that browsing is not possible.
3. Clicking a drive opens the "Upload & Share" page: the large drag-and-drop upload area comes first, and file management is expanded by default, but can also be collapsed manually; each time you enter a drive it is expanded again. The root directory in the breadcrumb is the Key's authorized root for that drive, not OpenList's mount path. The file list is paginated server-side; filtering applies only to the current page.
4. Select or drag files into the current directory. When you switch directories, the path navigation and upload target hint update in sync; while a directory is loading or if loading fails, the file statistics of the previous directory are not reused. The upload queue pins the target drive and directory chosen when the files were selected, so switching directories will not redirect in-progress tasks to another location. Name collisions are rejected by default and are not overwritten automatically.
5. After an upload completes, the task in the upload records offers a prominent "Get link" button: clicking it opens the share dialog, and only after you choose a validity period and confirm "Generate link" is a public download link created (you can also use "Share" on that file in file management). The upload itself never makes a file public automatically; the recipient downloads via the link and needs no API Key.

"My" denotes the current Key's authorization scope; it does not add multi-user registration, personal passwords, user quotas, or an independent account system. No Key is created or widened automatically, and admin login is not treated as file access authorization. If a Key's plaintext is lost, it must still be rotated by an administrator; the workbench cannot recover it.

## Permissions and operations

| Operation | Required permission |
| --- | --- |
| View a directory, enter a folder | list |
| Download a file | read |
| Upload, create a directory, rename | write |
| Delete | delete |
| Create a public download link | link |

Effective permissions are the union of the current Key's global permissions and the per-drive permissions of **authorized drives**; a global permission does not make an unauthorized drive automatically visible. Disabling a button in the frontend is only interactive feedback; every server-side operation still verifies the Key, root directory, quota, drive identity, and the corresponding permission. Do not bypass a disabled button by giving a day-to-day Key the "All" option.

A general-purpose Key does not include delete, but write can still rename, move, or overwrite under an explicit conflict policy, so it must not be understood as a non-destructive permission. Deletion requires explicit confirmation, and results are shown per item according to server-side status; whether an item goes to the recycle bin depends on the existing Key policy, so recovery on an arbitrary drive cannot be guaranteed. The workbench does not provide purge.

## Upload and download results

Uploads use the existing designated-drive file interface, and the raw file stream is sent directly to the gateway; empty files use multipart to avoid the raw request body's requirement of a positive Content-Length. After the browser's send progress reaches 100%, it shows a waiting-for-confirmation state rather than immediately showing completion. The upload is marked complete only after a valid upstream confirmation response, target path, size, and checksum fields all match the agreement.

When the network drops, a request is cancelled, or a valid confirmation is missing, the state is shown as interrupted / result unconfirmed; the remote side may already have written the data, so you cannot automatically retry, overwrite, or claim a rollback on this basis. The queue does not persist across a page refresh; logging out clears memory and cancels this page's requests. A client-side cancellation only stops waiting or sending and cannot undo a write already completed upstream.

For downloads, the page first requests a **short-lived, single-file download credential** through the workbench's same-origin interface (the server checks the read authorization of the current Key restored from the session), and then the browser's native downloader receives the server's streaming response. The web page does not collect the whole file and does not generate a Blob, and it no longer depends on the File System Access API, an HTTPS secure context, or the browser's local file picker; large files no longer consume whole-file memory through this frontend path. The API Key does not enter the download URL, and no public share link is created for an ordinary download.

The page only reports "handed to the browser for download" and cannot claim on this basis that the download or local save has completed. Progress, cancellation, failure, and actual completion are managed by the browser's download list; logging out of the workbench cancels credential requests that have not yet completed, but cannot cancel downloads already handed to the browser. The download credential itself also carries holder access capability during its brief validity period, so do not forward it; for long-term or deliberate sharing, still use the separate "Share" operation.

## Public sharing

- Clicking "Get link" (for a completed task in the upload records) or "Share" in file management only opens a dialog; only after you confirm "Generate link" inside the dialog is a public download link created, and an upload never makes a file public automatically. Without the link permission, the upload area clearly states that a public link cannot be generated.
- The validity period is optional and does not exceed 7 days; use the public address and expiry time returned by the server.
- A link carries holder access capability: anyone who knows the link can download. Before sending it, confirm that the file is allowed to be public, and do not treat the link as private authentication.
- Link expiry does not delete the original file; the download may fail if the file is moved or deleted, if the Key is revoked or expires, or if the drive is unavailable.
- Native links currently provide no password, download-count limit, access statistics, or per-link revocation UI. Do not apply Gokapi's temporary relay count and retention policy to native drive links.
- The share URL depends on the instance's public access address setting. Before sending it to outsiders, an administrator should correctly configure the public address, reverse proxy, and HTTPS; the page will not open ports or change the network on the administrator's behalf.

## Security boundaries and implementation

The workbench adds no file-operation backdoor and no administrator proxy. Ordinary operations continue to use the API Key data plane; native downloads separately use a short-lived single-file credential that hands the original Key's existing read authorization to the browser's downloader, without creating an independent or larger file authorization.

- The static page and allowlisted resources load without a session; responses set no-store, a strict CSP, nosniff, and no-referrer. No admin cookie is set; the file session cookie issued at login is an opaque random token with Path limited to `/files`, HttpOnly, and SameSite=Strict, and the server stores only its hash.
- After login the raw Key is not kept in page memory, localStorage, or sessionStorage, and never enters a URL; the Key submitted at login is cleared as soon as the request completes. All file UI APIs (including reads) go through the same-origin `/files/w/<id>/api/v1/<suffix>` adapter with the `X-C2A-Workspace: 1` header and `credentials: same-origin`; the server restores the session to the verified current Key and hands it to the existing `/v1` dispatch. The machine-facing `/v1/*`, legacy APIs, and the relay do not accept this cookie, and the session cannot reach administration interfaces.
- A browser holds at most 16 valid workspaces; each has a fixed 30-day absolute validity without sliding renewal. On every restore and call the server rechecks the current Key's existence, revocation, expiry, and credential version; permissions, roots, quotas, rate limits, audit, and the storage guard follow the current Key in real time, with no cached authorization snapshot. A failed check invalidates only that workspace, not the others.
- The session cookie is marked `Secure` for direct TLS connections or when the administrator has explicitly configured a trusted HTTPS `public_base_url`; an HTTPS reverse proxy should set `public_base_url` correctly. HTTP deployments provide no transport encryption; use HTTPS for public exposure.
- Logout, authentication failure, and page leave clear page state and cancel the requests managed by this page; `pagehide` only clears memory and does not sign the persistent session out server-side. Asynchronous responses are checked against the login and directory generation, so the previous Key's listing, share results, or a late download credential cannot be used for the next Key. Tasks already handed to the browser's downloader continue to be managed by the browser.
- The download credential is bound to the Key, the drive, a single file, the authorized root, and the storage identity, is valid for about 120 seconds, and can be retried or used for Range downloads within its validity period. Each redemption rechecks the current Key and read authorization, and an old credential may not be used to bypass revocation, expiry, or a change in authorization. A valid stream is not actively cut off at 120 seconds; the credential does not depend on process memory and is still verified after a restart with the same persistent signing key, while rotating the signing key invalidates old credentials. Do not record the full credential URL in reverse-proxy access logs.
- File names, paths, errors, and the like are all displayed as text; drive content is not run as HTML or script.
- `cloud2api only links to the patch pages; it never receives credentials or writes storage config.` This refers to the login credentials and configuration of drives and upstream storage; the workbench handles its own data-plane API Key as its responsibility requires.
- `/relay/` retains its separate temporary drop-box purpose, and its compatibility routes are not migrated or removed. The workbench does not connect to its hidden `.relay` directory.
- No albums, media/Office previews, user registration, or the OpenAI model protocol are added, and the Agent base-layer interface discovery manifest is not changed.

Entry points and static assets: the navigation page `web/home.html`, the file page `web/files.html`, `web/assets/files.js`, and `files.css`, assembled by `internal/admin`; workspace login and session APIs live in `internal/agentapi/workspaces.go`, with the persistent session tables in `internal/store`. File data is still handled by the existing `/v1` of `internal/agentapi`.

## Development verification

`/files/?mock=1` uses separate in-memory demo data. Drives, files, uploads, and sharing all do not call the real data plane and issue no persistent session cookies; demo changes are not retained after logging out or refreshing. Do not treat links or completion states in the demo as evidence of a real drive test.

```sh
go test ./internal/admin ./internal/agentapi ./internal/store
node scripts/test-files-page.cjs
node scripts/test-agent-onboarding.cjs
```

End-to-end acceptance is performed only against isolated simulated/Local storage. Write-type self-checks, deletion tests, fault injection, or rebuilding fixtures must not be run against a user's real drives. Deployment still requires a separate current-inventory check, a consistency backup, and rollback preparation.
