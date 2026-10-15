[简体中文](API.md) | [English](API.en.md)

# connect module interface

The patched OpenList adds the following interfaces to its own HTTP service. The conventions match OpenList: the HTTP status is always 200, the response body is `{code, message, data}`, and `code` of 200 means success; on failure, `message` is a directly presentable Chinese explanation and `data.error_code` is the machine-readable code in the table below.

The admin interfaces hang under `/api/admin/connect/` and pass through OpenList's own `Auth` and `AuthAdmin` middleware: a browser can use an admin session the OpenList-native way (`localStorage.token` as the `Authorization` header), or (from connect 4) use the Cookie issued by passwordless handoff — that Cookie is `HttpOnly`, `SameSite=Strict`, has a 30-minute absolute expiry, and its `Path` is limited to `/api/admin/connect`, see [Passwordless handoff](#passwordless-handoff-connect-4) below. Both methods take effect only for `/api/admin/connect/*`, and the wizard does not write the admin token into browser storage. A login session is also bound to the initiator's user ID and `Authorization` fingerprint: another token of the same user (for example the permanent token held by cloud2api) cannot see a session initiated by the browser.

| `error_code` | `code` | Meaning |
| --- | --- | --- |
| `invalid_input` | 400 | A parameter is missing or malformed |
| `app_missing` | 400 | This platform requires an app to be configured first |
| `app_invalid` | 400 | The app configuration is incomplete or does not match the mode |
| `unsupported` | 400 | The adapter does not support this operation, for example reauthorising a SharePoint storage |
| `state_mismatch` | 400 | The OAuth state does not exist, has been used, or does not belong to this session |
| `not_found` | 404 | The platform, session or storage does not exist (someone else's session also returns 404) |
| `conflict` | 409 | Concurrent modification, duplicate processing, or a disallowed state |
| `expired` / `cancelled` | 410 | The session or QR code has expired or been cancelled |
| `limit` | 429 | Too many sessions in progress or too many requests |
| `provider` / `network` | 502 | The cloud-drive service rejected the request / cannot be reached |
| `internal` | 500 | Internal error |

## Platforms and app configuration

### `GET /api/admin/connect/platforms`

```json
{
  "module": {"name": "connect", "version": "4", "upstream_version": "v4.2.6",
             "upstream_commit": "2bdf16d", "web_version": "v4.2.6", "handoff": true},
  "callback_path": "/api/connect/oauth/callback",
  "suggested_redirect_uri": "",
  "platforms": [{
    "id": "baidu", "name": "百度网盘", "driver": "BaiduNetdisk", "method": "qrcode",
    "app_modes": ["public_app", "online_service", "own_app"],
    "mode_methods": {"online_service": "form", "own_app": "oauth"},
    "public_app_label": "api.oplist.org「百度网盘 - 手动登录」使用的公开应用", "form_label": "粘贴令牌",
    "scopes": ["basic", "netdisk"], "pkce": false, "allow_oob": true,
    "form_fields": [{"key": "refresh_token", "…": "…"}], "root_field": "root_folder_path", "root_default": "/",
    "root_label": "根目录路径", "root_help": "…", "reauth_fields": ["refresh_token", "…"],
    "notes": ["…"], "verified": "", "docs_url": "https://doc.oplist.org/guide/drivers/baidu",
    "active_method": "qrcode",
    "app": {"required": true, "default": true, "configured": true, "mode": "public_app",
            "identity": "NqOM…G0Na", "client_secret_set": false, "problem": ""}
  }]
}
```

- App modes (`app_modes`): `none` (no app needed, e.g. 123pan), `public_app` (the public app built into the adapter, which the driver uses to renew directly with the cloud drive), `online_service` (the OpenList online authorization service, through which the driver renews), `own_app` (the administrator's own developer app). The first item is the default mode.
- The login method varies with the mode: `method` is the login method of the default mode, `mode_methods` lists the different methods of other modes, and `active_method` is the method the platform is actually using (the mode of the saved configuration, or the default mode).
- When no app configuration is saved, platforms whose default mode is `none`, `public_app` or `online_service` (whose address is `https://api.oplist.org`) are directly usable, with `app.default` being `true`; platforms whose default mode is `own_app` return `{"required": true, "configured": false, "mode": "", "client_secret_set": false, "problem": ""}` and require configuration first. None of the first platforms require configuration first.
- Platforms that need no app: `app` is `{"required": false, "configured": true, "mode": "none"}`.
- The credentials of `public_app` come from the adapter; the response contains no `client_id`, `redirect_uri`, `revision` and similar fields, and `identity` is the masked client_id.
- `callback_path` already includes OpenList's base path; a page for a user-owned app uses `location.origin + callback_path` as the suggested callback address. When `site_url` is set in the configuration file, `suggested_redirect_uri` is the full address generated from it.
- A non-empty `problem` means the saved configuration currently cannot be used (the reason the adapter validation failed).
- App secrets are write-only: the response has only `client_secret_set`. The `client_id` of a user-owned app is not a secret, and the administrator needs to see the full value when editing.
- `module.handoff` (from connect 4, boolean) indicates that this module supports passwordless handoff; older modules have no such field, which cloud2api treats as unsupported and falls back to opening the wizard link manually.
- cloud2api reads only `version/upstream_version/handoff` of `module` and `id/name/driver/method` of `platforms[]`, to determine whether OpenList carries this module, which drivers can be reauthorised, and whether handoff can be initiated.

### `POST /api/admin/connect/app/save`

Request `{"platform", "mode", "client_id", "client_secret", "redirect_uri", "service_url"}`. When `client_secret` is an empty string, the saved value is kept (only for the same mode and same client_id). When `mode` is `public_app`, the remaining fields are ignored and only the mode is saved. The adapter `CheckApp` validates first, and only on success is it written; each save increments `revision` by one. Returns the `app` object of that platform.

### `POST /api/admin/connect/app/delete`

Request `{"platform"}`, returns the `app` object after deletion: a platform with a default mode returns to its default. Sessions already initiated keep using the app snapshot at their start, and existing storages keep their own renewal parameters; neither is affected.

### `GET /api/admin/connect/mount/suggest?platform=baidu`

```json
{"mount_path": "/百度网盘"}
```

The suggested mount path when creating a storage: the platform name (with `/` and control characters removed), trying `-2` through `-99` in turn when occupied by an existing storage, and an empty string when all are occupied. The page fills it in when the mount path is empty on the "Confirm and save" screen. An unknown platform returns `not_found`.

## Reauthorization target

### `GET /api/admin/connect/target?id=12`

```json
{"storage_id": 12, "mount_path": "/baidu", "driver": "BaiduNetdisk", "status": "work",
 "disabled": false, "modified": "2026-10-14T08:00:00Z",
 "platforms": [{"id": "baidu", "name": "百度网盘", "problem": ""}],
 "edit_path": "/@manage/storages/edit/12"}
```

`platforms` is the adapters whose driver matches this storage; `problem` explains why reauthorization is temporarily unavailable (no app configured, the storage is disabled, the adapter does not support this storage's configuration, etc.). When it is an empty array, the page guides the user to `edit_path` to use the OpenList native form.

## Login sessions

Sessions live only in the OpenList process memory and are lost on restart. Phases (`phase`):

| Phase | Description |
| --- | --- |
| `pending` | Waiting for the user to authorise, scan a code, or submit a form |
| `authorized` | Credentials have been staged on the server, waiting to choose a mount point and save |
| `saving` | The configuration adaptation layer is writing |
| `saved` / `save_failed` | Saved / write failed (see `result`); in both cases the credentials have been cleared |
| `failed` | The login itself failed (cloud drive rejected, callback error, etc.) |
| `cancelled` / `expired` | Cancelled / expired; the credentials have been cleared |

Time limits: `pending` 10 minutes; save within 15 minutes after `authorized`; the result of a finished session is retained for 30 minutes for repeated queries. Each administrator has at most 5 sessions in progress.

### `POST /api/admin/connect/session/start`

Request `{"platform": "baidu"}`, add `"reauth_storage_id": 12` when reauthorising. Returns the session view (View):

```json
{
  "id": "…", "platform": "baidu", "platform_name": "百度网盘", "driver": "BaiduNetdisk",
  "method": "oauth", "mode": "create", "target": null, "phase": "pending",
  "app": {"mode": "own_app", "identity": "abcd…wxyz", "scopes": ["basic", "netdisk"]},
  "oauth": {"authorize_url": "https://openapi.baidu.com/oauth/2.0/authorize?…",
            "redirect_uri": "http://…/api/connect/oauth/callback", "oob": false},
  "qr": null, "form": null, "auth": null, "result": null, "error": "", "error_code": "",
  "root": {"field": "root_folder_path", "default": "/", "label": "根目录路径", "help": "…"},
  "notes": ["…"], "verified": "", "created_at": "…", "expires_at": "…"
}
```

- The example above is a Baidu session in own-app mode. `method` is the login method of this session, determined by the app mode at the session's start (see `mode_methods`): when no app configuration is saved, the platform default mode is used (Baidu's default is public-app QR scan); when the default mode is `own_app` and it is not configured, `app_missing` is returned.
- `qr` (QR scan): `{"status": "waiting|scanned|confirmed|cancelled|expired|network_error", "image_url", "content", "expires_at", "message", "can_refresh"}`. `image_url` is the QR image provided by the cloud drive (the page loads it with `referrerpolicy="no-referrer"`); when there is no image the page displays `content` as text, and the QR adapters of the first platforms (Aliyun Drive, Baidu Drive public app) both provide an image. `message` is initially the hint carried by the QR code (for Baidu: "…you can also open https://openapi.baidu.com/device in a desktop browser and enter the code…"), is replaced by the status explanation returned by the cloud drive, returns to that hint after a network error recovers, and is cleared after the QR code expires.
- `form` (form): an array of fields `{key, label, type, required, secret, default, options, help, create_only}`; the `create_only` fields are not present when reauthorising.
- `form_label`: the name of the form login, for example "Paste token"; when empty the page displays "Account password".
- `guide` (some form logins): `{"url", "label", "steps": [...]}`, the external page to open before submitting and the steps to follow. The "Paste token" of Baidu, OneDrive and Google Drive points to the OpenList online authorization page; the Baidu and OneDrive addresses carry a `#` parameter preselecting that cloud drive's option, while Google carries none (keeping that page's Google usage pop-up).
- `auth` (after `authorized`): `{"credential_type", "app_mode", "app_identity", "renewal", "account", "fields": [...], "create_fields": [...]}`, with field names only, no values.
- `result` (after saving): see below.

### `GET /api/admin/connect/session/get?id=…`

Returns the current view. A QR session queries the status from the cloud drive here, and the server queries each session at most once every 2 seconds, independent of the page's polling frequency; a network error is shown as `network_error` and polling continues until the QR code expires.

### `POST /api/admin/connect/session/submit`

Request `{"id", "values": {…}}`:

- Form login: the value of each field. On a validation failure the session stays `pending` and can be amended and resubmitted.
- Paste token: `{"refresh_token": "…"}`. It accepts the token itself, JSON containing `refresh_token`, or an authorization-page address whose `#` is followed by base64-encoded JSON; leading and trailing whitespace is stripped, and `invalid_input` is returned when there is whitespace in the middle, or the length is less than 16 or greater than 8192. The server does not check whether the token itself is valid: the driver validates it during initialisation when saving, and the storage created this time is deleted on failure.
- When the OAuth callback address cannot be opened: `{"callback_url": "<the full address in the browser address bar>"}`, whose state must belong to this session.
- When the app's `redirect_uri` is `oob` (Baidu only): `{"code": "<the authorization code shown on the page>"}`.

### `POST /api/admin/connect/session/refresh`

Request `{"id"}`, a QR session regenerates the QR code, at most 5 times per session.

### `POST /api/admin/connect/session/cancel`

Request `{"id"}`. A `pending` or `authorized` session becomes `cancelled` and its credentials are cleared; `saving` returns `conflict`; an already finished session is returned unchanged.

### `POST /api/admin/connect/session/save`

Request `{"id", "mount_path", "root", "remark"}`; reauthorization ignores the last three. It is executed only once per session: a duplicate or concurrent request waits for and returns the same result. `result`:

```json
{
  "ok": false, "mode": "create", "storage_id": 13, "mount_path": "/baidu", "status": "",
  "message": "驱动初始化失败，已删除本次创建的存储 #13",
  "error": "failed init storage: …", "retryable": false, "credentials_written": true,
  "cleanup": {"attempted": true, "deleted": true, "storage_id": 13, "error": ""},
  "restore": null
}
```

- `retryable: true`: nothing was written (the mount path was already occupied, the path format was invalid, etc.), the session stays `authorized`, and it can be saved again after amendment.
- Create failure: `cleanup` reports whether **this** created storage was deleted (by the returned storage ID and after verifying the mount path and driver, re-reading after deletion to confirm).
- Reauthorise failure: `restore` reports whether the original configuration was restored (`restored` is true only when a re-read confirms the original settings are back in place and the new credentials have been removed) and the state after restoration. `credentials_written` indicates whether the new credentials were ever written to the storage.

## OAuth callback

### `GET /api/connect/oauth/callback`

After the cloud drive authorization is complete, the browser is redirected here (the callback address registered in the app configuration). It does not pass through OpenList login: a one-off state associates the session, and PKCE (OneDrive, Google) binds the authorization code. The server exchanges the authorization code immediately, then returns a result page without credentials or authorization code, prompting the user to return to the original page and continue; when the state is unknown or already used, a 400 page is returned.

The authorization code appears in the request address of the OpenList access log. It can be used only once and is exchanged immediately on callback, and the exchange also requires the app secret (and PKCE verifier) saved on the server, so the authorization code in the log can no longer be used; when this must be avoided entirely, the path can be excluded from the log filtering in the OpenList configuration.

## Passwordless handoff (connect 4)

An administrator can open the login wizard directly from the cloud2api portal without first logging in to OpenList manually. The flow: the cloud2api backend calls `handoff/start` to obtain a one-off ticket, the wizard address carries the ticket and the target in the URL fragment; after reading it the page **immediately clears the fragment** and exchanges the ticket for a Cookie limited to the connect admin interfaces. At no point is an admin token passed through a redirect link, and the OpenList admin password, the full admin JWT or a long-term admin token never appears in browser storage; the native `/@manage` admin backend is not in the passwordless scope. The interfaces in this section have passed component-level tests, pairing acceptance and final real-browser verification (passwordless entry, cancellation, logout revoking the Cookie, cross-site return; GUI upload and the native pop-up click flow are not in the verification scope), and went live with the local deployment of 2026-10-15.

### `POST /api/admin/connect/handoff/start`

Passes through `Auth` and `AuthAdmin`, called by the cloud2api server (which holds an admin token); the browser does not call it directly. The request body is an empty JSON `{}`. Response `data`:

```json
{"code": "<32-byte base64url>", "expires_in": 60}
```

- `code` is a random 32-byte base64url encoding (unpadded), valid once and expiring within 60 seconds; `expires_in` is the remaining seconds (1–60). The ticket is kept only in the OpenList process memory and is lost on restart.

### `POST /api/connect/handoff/exchange`

A public endpoint that does not require login. The request carries `{"code": "<ticket>"}`: when the ticket is valid, a Cookie is issued (`Path=/api/admin/connect`, 30-minute absolute expiry, `HttpOnly`, `SameSite=Strict`; the value is a session token limited to this module, not an admin JWT). When the ticket has been used, expired or is malformed, an error is returned per the table above and no Cookie is issued. On a successful exchange the page opens the target carried in the fragment; that Cookie is used only to access `/api/admin/connect/*`. Session query and logout:

| Interface | Description |
| --- | --- |
| `GET /api/admin/connect/auth/me` | Returns the user and capabilities of the current passwordless session (whoami) |
| `POST /api/admin/connect/auth/logout` | Logs out the passwordless session and clears the Cookie |

The wizard address's fragment has the form `#handoff=<ticket>&target=/new/goapi`, and `target` accepts only `/`, `/new/<platform>`, `/app/<platform>`, `/reauth/<storage_id>`; the cloud2api side has already validated the target against the `platforms` list before initiating, and the page side validates it just as strictly (the platform after `/new/` and `/app/` must exist, and the value after `/reauth/` must be a positive integer). The fragment is cleared immediately on page load and appears nowhere beyond the history; the ticket itself is one-off, so reopening an old address only yields an invalid message.

## Pages

`GET /@connect/` is the minimal login configuration page, containing only `index.html`, `app.js` and `app.css`, with no file preview, gallery, media playback or Office preview code, and loading no external scripts. Page routes:

| Address | Purpose |
| --- | --- |
| `/@connect/` | Platform list and app configuration entry |
| `/@connect/#/app/<platform>` | App configuration |
| `/@connect/#/new/<platform>` | Create storage |
| `/@connect/#/reauth/<storage_id>` | Reauthorise (cloud2api's entry) |
| `/@connect/#/s/<session_id>` | Return to a session in progress (linked from the callback result page) |

Any address can carry the query parameter `return`, the cloud2api page to return to after completion, for example `/@connect/?return=http%3A%2F%2F127.0.0.1%3A8080%2Fadmin%2Fconnect-return%3Fstorage_id%3D%7Bstorage_id%7D#/new/baidu`. The page accepts only addresses of length at most 2048 that parse, whose protocol is http/https, that contain no username or password, and whose host name is the same as this page or a local loopback address (`localhost`, `127.x.x.x`, `[::1]`, when the portal is opened at 127.0.0.1 while this page is opened at a LAN address); other values are ignored. After a successful save the primary button is "View in cloud2api", with `{storage_id}` in the address replaced by the storage ID saved this time; the save-failure page and the home page show "Return to cloud2api", and when no storage ID has been obtained (save failure, cancellation, ticket expiry, etc.) it returns the relay-page address without query parameters, which then redirects back to the portal list `/admin/#/drives`. All login wizard links of the cloud2api portal carry this parameter.

The `return` passed in by cloud2api points to its own return relay page `GET /admin/connect-return`, not a direct portal hash address: OpenList is often accessed at a LAN address while the portal is at a loopback address, so redirecting straight back to the portal is a cross-site navigation and the portal's `SameSite=Strict` session Cookie would not be sent. The relay page first loads a static page on the portal origin (CSP limited to self, `no-store`, no Cookie issued), then a script performs a same-origin redirect: with a `storage_id` it redirects to `/admin/#/drives?connected=<storage ID>`, and without query parameters it redirects to `/admin/#/drives`; this same-origin navigation can then carry the session, and the SameSite policy is not relaxed for it. This page needs no authentication and is not a login entry; with a `storage_id` parameter it accepts only a single positive integer, and an empty value, a format mismatch, or duplicate or extra query parameters return 400.

When creating, after authorization is complete there is a "Confirm and save" screen: the credential summary, the mount path (prefilled with the suggestion from `mount/suggest`), the root directory and a remark; reauthorization only confirms the target storage and then saves.
