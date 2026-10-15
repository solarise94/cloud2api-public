[简体中文](API-DEVELOPMENT.md) | [English](API-DEVELOPMENT.en.md)

# Development API layering and the complete protocol

This document is for project maintainers, administrators, and advanced integration developers; **it is not the default onboarding guidance handed to Agents.**

## 1. Layering conventions

| Layer | Audience and entry points | Behavior |
| --- | --- | --- |
| Agent base layer | Online instructions, Skill, OpenAPI, the `/v1` index, and the basic CLI | Introduces only routine file operations and confirmation of their results |
| Advanced and compatibility layer | This document, the complete protocol snapshot, and the repository's full CLI | Retains the implemented capabilities without promoting them in default Agent prompts |
| Administration and connection layer | The admin portal, the OpenList wizard, and project maintenance documents | Administrators connect and onboard storage and configure Keys and permissions; this is not the Agent data plane |

**Layering does not change authorization, and it is not a security isolation or a routing switch.** Clients that know an advanced URL can still call it, but must still pass the original Key, drive scope, root directory, permission, expiry, quota, and storage-identity checks. The administration API keeps administrator-session authentication and does not accept a routine data-plane Key as a substitute.

The existing global permissions and per-drive permissions are still combined as a union. That the default material does not introduce a given capability does not mean it has been revoked from the Key, nor that all operations in the base set have been granted automatically. For example, the "General" permission group does not include delete, and the appearance of delete in the basic documentation does not give that Key deletion rights.

The complete Agent/compatibility protocol snapshot is at [`openapi.full.json`](openapi.full.json). It is kept in the repository and does not additionally publish an online full-spec discovery entry point. It covers the protocols registered in `routeTable` and does not include administration routes, OpenList's native protocol, or every historical gateway interface.

## 2. Agent base layer: 11 paths, 12 operations

| Method | Path | Purpose |
| --- | --- | --- |
| GET | `/v1/me` | Discover authorized drives, root directories, effective permissions, and health status |
| GET | `/v1/drives/{drive}/list` | List a directory with pagination |
| GET | `/v1/drives/{drive}/stat` | Get metadata for a single object |
| GET / PUT | `/v1/drives/{drive}/files` | Stream a download / upload |
| POST | `/v1/drives/{drive}/mkdir` | Create a directory |
| POST | `/v1/drives/{drive}/delete` | Delete a path, handled per the Key's recycle-bin policy |
| POST | `/v1/drives/{drive}/move` | Move or rename within the same drive |
| POST | `/v1/transfers` | Copy or move across drives, returning an asynchronous job when needed |
| GET | `/v1/jobs/{id}` | Query or long-poll a job result |
| POST | `/v1/links` | Create a time-limited share link |
| GET | `/l/{token}` | Download via a share link, without carrying an API Key |

The base layer uniformly introduces the drive ID in the path. `drive` is URL-encoded as a single path segment, while a file `path` is encoded as a query parameter or a JSON field. A file path is always inside the Key's root for that drive and does not include OpenList's internal mount prefix.

`/v1/me` does not hide the real advanced permissions or compatibility metadata, and keeps capability fields such as `relay_hop_limit` used by the GoAPI upstream driver; it only removes operational guidance toward advanced interfaces and does not falsify permissions. `next_steps` may still indicate that a drive is unavailable or that an existing job is pending, but it no longer suggests automatically running selftest, restoring the recycle bin, or opening the connection-management flow.

The default Agent documentation does not introduce both the default-drive and designated-drive file URL forms at the same time, and does not suggest writing a test file after connecting. Authentication and read-only discovery start from `me`, and an actual write happens only when a user task needs it.

### Result and retry constraints

- A native upload must check the response and verification results such as size, CRC-64/XZ, and SHA-256; merely finishing sending the request body or receiving an HTTP 200 does not replace a completion confirmation.
- `202` and a job ID only mean the task has been accepted. Call `GET /v1/jobs/{id}?wait=N`, check the status and errors, and do not report success directly.
- The `dry_run` and conflict policies of delete, move, and transfers are retained; rehearse large-batch or large-volume operations first.
- `Idempotency-Key` must not be interpreted as permission to blindly retry any failure. An uncertain result does not mean nothing was written, nothing was moved, or a rollback happened.
- Permanent purge differs from ordinary deletion; there is no need to suggest switching to the "All" permission group just because the base material introduces delete.

## 3. Retained advanced interfaces

See the protocol snapshot and the corresponding source code for the complete request fields. The following records the purpose, permission boundaries, and maintenance notes, without adding them to the default prompts.

| Capability | Interface | Maintenance notes |
| --- | --- | --- |
| Duplicate drive discovery | `GET /v1/drives` | Returns a drive view generated by the same function as `me.drives`; kept for compatibility |
| Default-drive file aliases | `/v1/list`, `stat`, `files` GET/PUT, `mkdir`, `delete`, `move` | Resolved by the Key's default drive; overriding via `?drive=` is not supported |
| Directory tree | `GET /v1/tree`, `GET /v1/drives/{drive}/tree` | Has depth and item caps and cannot traverse without bound |
| Search | `GET /v1/search`, `GET /v1/drives/{drive}/search` | Works from an index or a bounded traversal and does not guarantee full-text search at any scale |
| URL transfer | `POST /v1/fetch` | A separate fetch permission and server-side URL safety restrictions; usually returns a job |
| Batch | `POST /v1/batch` | At most 100 items, authenticated per item; a cross-drive copy may become an asynchronous job |
| Job list | `GET /v1/jobs` | Only the current Key's jobs |
| Job cancellation | `POST /v1/jobs/{id}/cancel` | Best-effort cancellation of the underlying task; does not guarantee that an already-completed remote write is rolled back |
| Recycle-bin list | `GET /v1/trash` | Only the records visible to that Key; the ordinary file API does not directly address the internal trash directory |
| Restore | `POST /v1/trash/restore` | Requires the delete permission and the original drive authorization; restore conflicts must still be handled |
| Permanent purge | `POST /v1/trash/purge` | A separate purge permission; must not be confused with ordinary deletion |
| Child Keys | `GET/POST /v1/keys`, `DELETE /v1/keys/{id}` | Requires global delegate; subset authorization, restricted TTL, and revoking the parent Key cascades to revocation |
| Activity records | `GET /v1/activity` | Only the current Key; returns logical paths and does not reveal internal mount paths |
| Write self-check | `POST /v1/selftest` | Has side effects; it is not liveness/readiness and is not a necessary step for routine connection |

For links and fetch, the designated drive is taken from the request body; for transfers, the source and destination each have a drive; batch can specify per item. Do not incorrectly generalize this request-body convention into a claim that all file interfaces support a drive query parameter.

### selftest in particular

The current self-check verifies write permission per drive by writing a small file inside the authorized root, reading it back for verification, and then having the server clean it up. The cleanup does not require the calling Key to have the permission that the ordinary delete interface requires; a client that separately calls delete still needs its own delete authorization.

In the current response, failures are reported in the per-drive results and are usually still HTTP 200. A skipped drive without write does not necessarily set `all_ok` to false, so skipping every drive also cannot be taken as a successful read/write verification. Future changes should clearly define passed/failed/skipped and the number of tests, and must not automatically widen permissions to make the self-check turn green.

## 4. Gokapi-compatible relay: kept separate

This is a group of temporary delivery, sharing, and receipt interfaces addressed by file ID, compatible with existing Gokapi-style clients, and used by this project's separate `/relay/` page. It is not the OpenList GoAPI upstream mount protocol.

| Scope | Interface |
| --- | --- |
| Service info | `GET /api/info/version`, `GET /api/info/config` |
| Upload | `POST /api/files/add` |
| Chunking | `POST /api/chunk/add`, `POST /api/chunk/complete` |
| Upload receipt | `GET /api/files/status/{uploadId}` |
| File metadata | `GET /api/files/list`, `GET /api/files/list/{id}` |
| Delete and cancel delayed deletion | `DELETE /api/files/delete`, `POST /api/files/restore` |
| Authenticated download | `GET /api/files/download/{id}` |
| Share download and pages | `GET /downloadFile?id=…`, `GET /d?id=…`, `GET /h/{hotlinkId}` |
| Legacy compatibility onboarding material | `GET /custom/gokapi-agent-instructions.txt` |

Compatibility onboarding material is retained for clients that explicitly use the old protocol, but is not linked from the basic instructions, Skill, index, or `me`. Its existence does not mean the default Agent should load it automatically.

Temporary files are isolated under `.relay` beneath the authorized root, have quotas, download-count, and expiry policies, and are retained for at most 7 days. Receipts have the states `uploading/completed/failed/interrupted` and may only be queried by the original Key. A successful staging, a finished send, or a non-blocking submission is not completion; the state may be reported as completed only after the upstream has confirmed and the metadata has been persisted.

Deletion, restore, and share download do not authenticate in the same way, and these boundaries must not be merged just to shorten URLs. If the compatibility layer is removed in the future, `/relay/`, existing clients, and upload-receipt consumers must be migrated first. For the detailed policy, see [API Keys and relay](API-KEYS-AND-RELAY.en.md).

## 5. Administration and connection interfaces

The admin portal and the OpenList login wizard each have their own authentication boundaries; these routes do not enter the Agent's basic OpenAPI.

| Administration capability | Entry point and source |
| --- | --- |
| Portal administrator login and session (persisted in SQLite with a TTL from the `admin.session_ttl` setting, 24 hours by default; sessions survive restarts, and changing the administrator password invalidates existing sessions) | `/admin/`, `/admin/api/login`, `/admin/api/logout`; `internal/admin/session.go`, `portal.go` |
| Drive discovery, onboarding, sync, unbinding, and identity confirmation | `/admin/api/drives` and its per-drive subresources; `internal/admin/drives.go`, `internal/reconcile/` |
| Key creation, editing, revocation, rotation | `/admin/api/keys` and its subresources; `internal/admin/keys.go` |
| Settings, audit, and maintenance | The portal's corresponding `/admin/api/*`; `internal/admin/portal.go` |
| Minimal login wizard and GoAPI upstream connection | OpenList `/@connect/`; `openlist-patch/src/server/connect/`, `pkg/loginsdk/` |
| Administrator passwordless handoff | cloud2api `POST /admin/api/connect/handoff`; OpenList connect-only ticket and HttpOnly session |
| Wizard return to the portal | `GET /admin/connect-return`, a public fixed navigation page that issues no administrator session |

The flow is that an administrator initiates a connection in the portal, enters OpenList to complete login or configure an upstream, and then returns to the portal to onboard and authorize. cloud2api does not receive drive/upstream login credentials and does not write OpenList storage configuration; the upstream URL and Key are stored only in OpenList. Its own administrator password, data-plane API Keys, and server-side OpenList management credentials are still handled according to their respective responsibilities.

The GoAPI driver is used by OpenList over the remote `/v1` layered protocol, and its file operations overlap with the base layer, so these interfaces must not be removed just because the connection-management documentation is hidden. The `relay_hop_limit=4` capability declaration and the `X-GoAPI-Hop` loop-prevention protection must be retained.

See [OpenList patch notes](../openlist-patch/README.en.md), [patch API](../openlist-patch/docs/API.en.md), and [platform adapter development](../openlist-patch/docs/CONTRIBUTING-adapters.en.md). No Agent permission-request/administrator-approval interface is provided, and `/v1/requests` should not be reintroduced.

### Human file workspace

`/files/` is a human-facing file client, not a new administration interface. The instance root `/` is a public navigation page offering only the file workspace and the administration portal; `/files/` lists the workspaces still valid for the current browser and accepts adding a Key; `/files/w/<id>/` is the same static file page, where `id` is a random, non-secret entry identifier that can be bookmarked but does not authorize by itself. A single authorized drive goes directly to the file list after login, while multiple drives first show a selection page; an administrator session does not automatically become a data-plane Key, and no new whole-drive proxy is added for it. The static page and its allowlisted resources are assembled by `internal/admin`, workspace login and session APIs are handled by `internal/agentapi/workspaces.go`, and file operations still use the existing Key authentication and storage-identity guards. Usage and session boundaries are documented in the [file workspace guide](FILE-WORKSPACE.en.md). The default Agent prompts need neither loading the UI nor adding new operations.

Login and persistent sessions use a group of same-origin endpoints that serve only the page UI and are **not added to the Agent base manifest or the `routeTable` protocol snapshot count**:

| Method | Path | Boundary |
| --- | --- | --- |
| POST | `/files/api/login` | Signs in with an API Key and issues an HttpOnly session cookie (Path=/files, SameSite=Strict), returning `{workspace_id,url,expires_at}`; the Key is not echoed and the database stores only the token hash |
| GET | `/files/api/workspaces` | Lists only the workspace entries the current browser owns and that are still valid; returns no Key hashes or session tokens |
| POST | `/files/w/<id>/api/logout` | Removes only that workspace's session for the current browser, then returns to `/files/` |
| Any | `/files/w/<id>/api/v1/<suffix>` | Same-origin UI adapter (`X-C2A-Workspace: 1`) for what the page actually uses: me, directory/file operations, links, and download-ticket; it must not become a cookie proxy for all advanced/administration APIs |

Each workspace has a fixed 30-day absolute validity without sliding renewal; a browser holds at most 16 valid workspaces. The machine-facing `/v1/*`, legacy APIs, and the relay do not accept these cookies and still use only the Key header; the file cookie cannot reach administration interfaces either. On every restore and call the server rechecks the current Key's existence, revocation, expiry, and credential version; permissions, roots, quotas, rate limits, audit, and the storage guard follow the current Key in real time, with no cached authorization snapshot, and a failed check invalidates only that workspace.

Native browser downloads likewise use a UI-helper-only protocol:

| Method | Path | Boundary |
| --- | --- | --- |
| POST | `/v1/drives/{drive}/download-ticket` | Request body `{path}`, uses the API Key, requires read permission on that drive; returns `{url,expires_at}` |
| GET / HEAD | `/download/{token}` | Streams a download with a short-lived single-file credential, supports Range; each request rechecks the original Key and the current authorization |

The credential uses a separate purpose marker and an HMAC signature, is fixed at about 120 seconds of validity, and is bound to the Key, drive, file, authorization root, and storage identity; it contains no API Key plaintext. It differs from the public share of `/l/{token}`: requesting it does not need the link permission and does not create a persistent share. Retries and ranged requests are allowed within the validity period; expiry is only checked at the start of each request, and a stream that has already begun is not actively cut off because the credential expires. Revoking the Key, its expiry, removal of read authorization, or a change of root directory or storage identity all make subsequent redemption fail. The credential relies on a persistent signing key rather than process memory; a restart does not actively revoke it, while rotating the signing key invalidates old credentials. It returns no-store/no-referrer, and logs and audit records do not record the credential URL; a reverse proxy should likewise avoid recording or long-term retention of the token in that path. The browser is responsible for the actual download progress and result, and the page cannot treat submitting a native download as completed.

See [My files workbench](FILE-WORKSPACE.en.md) for the boundaries of usage, upload confirmation, public sharing, and browser credentials. `/relay/` keeps its separate temporary relay purpose; its download-count and expiry-deletion policies are not applied to native drive links, and the reference UI's password option is not claimed to be supported.

## 6. Full CLI and compatibility

- The default online `/custom/c2a.py` contains only the basic commands and basic interface references. When no drive is specified, it first resolves the default drive from `me`, then uses the designated-drive URL; link also supports `--drive`. The base CLI's cp exits 0 only after the job has succeeded; a failure, cancellation, or a still-incomplete state after waiting exits non-zero and keeps a query hint, without treating "accepted" as complete.
- The full CLI is in the repository at [`tools/c2a-full.py`](../tools/c2a-full.py) and retains the original advanced commands for developers and existing automation; it likewise uses the `C2A_URL` and `C2A_KEY` environment variables.
- Scripts that previously downloaded the online helper and then called tree/search/fetch/selftest/keys/activity/trash or jobs list/cancel need to switch to using the repository's full CLI. The underlying HTTP interfaces have not been removed.
- The default-drive HTTP aliases are still retained, but new Agent examples prefer the designated-drive path to reduce the burden of choosing between two forms.
- The old `/api/storage/*` and `/api/transfer` are not in the Agent base material; whether to remove them in the next major version is a separate compatibility decision.

## 7. Development and regression constraints

When adding an interface, you must make clear which layer it belongs to. The complete route table does not automatically equal the base allowlist, and a new advanced path must not enter the Agent tool manifest merely because of a generic `/v1/` prefix test.

- The basic OpenAPI and the `/v1` index use the same explicit method/path selection rules.
- The basic entry points, instructions, Skill, CLI help, and `next_steps` do not guide calls to advanced, compatibility, or administration endpoints.
- The complete protocol snapshot includes the supported designated-drive tree/search; the past figure of "53 paths, 56 operations" omitted those two, and after completing them it is registered as 55 paths, 58 operations. Administration endpoints and the historical gateway are still not in this count.
- Regression must simultaneously prove that advanced routes can still be used under their original authorization and that unauthorized calls are still rejected; it is not enough to test that "they are gone from the documentation."
- The effective permissions, root directory, quotas, and GoAPI capabilities of `me` must not be altered for visual concision.
- This round does not change the actual protocol of selftest, upload, asynchronous jobs, the recycle bin, or storage connection.

The complete snapshot is maintained by the full generator; to update it:

```bash
C2A_UPDATE_API_DOCS=1 go test ./internal/agentapi -run '^TestFullOpenAPIDocumentation$' -count=1
go test ./...
```

A normal regression should check that the snapshot is consistent with the generator. The example server uses the reserved documentation domain and does not write real deployment addresses, accounts, Keys, storage information, or runtime logs.

## 8. Design reference for future resource discovery

One can borrow the approach from CLI-to-API gateways of "provider registration, listing available resources, and selecting a target by a unified ID," treating different drives as unified resources rather than requiring the Agent to understand each drive's login protocol. The current `me.drives` already provides the authorized drive ID, provider, root directory, permissions, and health status, and can serve as the basis for such an interaction.

This is a design reference, **not a decision to switch drives to the OpenAI model protocol**. This round adds no `/v1/models` or `/v1/chat/completions`, and does not disguise file operations as inference requests. Even if other discovery appearances are added in the future, a resource ID can only select a target and cannot replace Key authorization; cross-drive operations still need the source and destination drives' permissions to be confirmed separately. The existing layered file API, the administrator connection flow, and the OpenList credential boundary remain unchanged.
