[简体中文](API-KEYS-AND-RELAY.md) | [English](API-KEYS-AND-RELAY.en.md)

# API Key permission groups and the API relay

Drive login and storage configuration are handled by OpenList; cloud2api only onboards storage, sets API Key authorization, and provides the file interfaces. Administrator password login is used for administration, and API Keys are used for restricted data-plane file operations; the two cannot substitute for each other. The “My files” workbench keeps its login through an HttpOnly session cookie (a fixed 30 days per workspace, with the server still authorizing against the current Key in real time), while `/relay/` issues no session and keeps the Key only in page memory; see the [file workspace](FILE-WORKSPACE.en.md) for its boundaries.

This document is project maintenance/advanced integration material and is not the default Agent prompt. The online Agent material shows only the basic file capabilities; the advanced interfaces, the Gokapi-compatible relay, and the administration connection flow still retain their implementations and original authentication, with the layering specification in the [development API documentation](API-DEVELOPMENT.en.md). Hiding this documentation does not revoke existing permissions, nor does it automatically open up capabilities that were not granted in the permission group.

## Common permission groups

When creating a Key, first choose the drive scope, the default drive/root, and the permission group; the fine-grained items are placed in "Advanced permission settings."

| Permission group | Includes | Excludes by default |
| --- | --- | --- |
| General (default) | list, read/download, write/upload, transfer, link, URL transfer, API relay | delete, permanent purge, child Key authorization |
| Read-only | list, read/download | write, relay upload, delete, share link, child Key authorization |
| All | All fine-grained permissions | None; choose it only when genuinely needed |

The API permissions corresponding to the fine-grained items are:

- General: `list read write transfer link fetch relay`.
- Read-only: `list read`.
- All: `list read write delete purge transfer link fetch delegate relay`.

"All" is still restricted by the selected drives, root directory, quota, and expiry, and is not administrator access. `write` allows uploading, overwriting, and moving files; that "General" does not include delete does not mean write operations carry no overwrite risk. A share link allows generating a time-limited address that can be downloaded without a Key, so "Read-only" does not additionally grant a share link by default. Enable it explicitly in the advanced settings when needed.

The custom permissions and per-drive root directories of old Keys are not changed to a permission group merely because the edit dialog is opened. Authorization changes only when a permission group is explicitly applied or a fine-grained item is modified. The static-configuration legacy expansion rules for old permissions remain compatible and do not bulk-rewrite existing Keys.

"Global permissions" in the advanced settings are an additional grant on top of the authorized drives, not a per-drive restriction: the effective permission is the union of the global permission and that drive's permission. Unchecking a drive's item alone cannot revoke the same permission that is still enabled in the global permissions; the global item must be unchecked as well. Global permissions do not automatically authorize unchecked drives and do not bypass the root directory.

## Single-drive, cross-drive, and the default root

- **Single drive**: authorizes only one specific drive, which is also the default drive.
- **Cross-drive**: explicitly checks currently onboarded drives, and the permission group applies to the selected scope; a default drive must be designated from the selected drives. Drives onboarded later do not automatically gain authorization.
- **Root directory**: the default `/` means the root directory of that OpenList storage; you can enter `/some-directory` to confine the Key to a subdirectory within the storage. It is not a host-machine path, nor a virtual root formed by merging multiple drives.
- **Default position**: short calls without a drive, such as `/v1/files` and `/v1/list`, target the default drive and are constrained by its Key root directory. Cross-drive calls still need an explicit source and destination drive, validated separately for read/write permission.
- **Advanced root**: each selected drive can have its own restricted directory. Switching the default drive should not empty or widen another drive's root directory.

Onboarding a drive itself does not add authorization to any existing Key. Permission changes are made only by an administrator editing a Key, and no agent permission-request or approval module is provided any longer. `/v1/requests` and the administration approval interfaces have been removed; retained old approval data no longer participates in runtime authorization.

## API relay login

`/relay/` is a separate local "relay upload tool" page that is not interlinked with the portal: neither the portal nor the home navigation page links to it, and the admin login page no longer offers an "API relay login" tab; open the address directly in a browser when needed. The page is explicitly labeled as a relay tool and provides only uploading, one's own file list, receipt queries, and download sharing, with no drive management, permission adjustment, or ability to view other Keys.

The Key is kept only in the current page's memory and sent via the `apikey` request header; it is not written into the URL, browser local storage, or a Cookie. Refreshing the page or logging out requires re-entering it. HTTPS is recommended for remote deployments; a plain HTTP LAN connection cannot prevent eavesdropping on the link.

The relay page shows the Key's actual relay target, scope, and capabilities, and reuses the existing Gokapi-compatible upload interfaces. Relay permission does not bypass the root directory, upload size, quota, storage identity, or expiry checks. Files are still stored on the storage connected by OpenList; this is not a second drive login service, and it also does not forward requests to a remote Gokapi service—the API upstream storage (`goapi`, connect 4) is accessed only through the layered `/v1` interfaces, and the Gokapi flat upload API is not used as an upstream protocol.

## How a model confirms the upload result

The Gokapi-compatible interfaces keep their original fields and add a queryable upload result. A successful chunk staging, the completion of sending an HTTP request body, and browser progress reaching 100% are all not the same as the remote file upload completing.

An upload call can carry a client-generated random ID:

```text
X-Upload-ID: <random upload ID>
```

The server returns the upload ID and records the result after receiving the file, on request interruption, and after the upstream response. Query it with the same Key:

```text
GET /api/files/status/<upload ID>
apikey: <API Key>
```

The meaning of the status field `Status`:

- `uploading`: still receiving, submitting, or waiting for the upstream; the model cannot claim completion.
- `completed`: the upstream upload has been confirmed successful and cloud2api's file metadata has also been saved; the file information and verification values may be reported.
- `failed`: a definite verification, quota, upstream, or metadata-save error was received; the model should state the failure reason and not hide the background error of a non-blocking upload. A failure does not guarantee that no file exists remotely, especially when the upstream has already received it but verification or local metadata saving failed; a rollback must not be claimed on this basis.
- `interrupted`: the request was disconnected, cancelled, or the service was interrupted, and the result may be incomplete; the remote side may already have a partial or complete file, and a rollback or non-existence must not be claimed.

When a client disconnects, it cannot receive the final response on the disconnected connection, and should use the known upload ID to query after reconnecting. No result or a timeout from the query only allows reporting "result unconfirmed"; it cannot be used to infer that the upload succeeded or to assert that no file exists remotely. Upload status may only be queried by the original Key and is not opened to other Keys just because the ID is known.

The model should return the terminal state, upload ID, file information, or failure reason to the user. When a non-blocking upload first returns `uploading`, it must wait for a subsequent query to obtain the terminal state; do not write "started processing" as "upload completed." Before retrying, first query the result of the original attempt to avoid duplicate submission or overwriting. `X-Upload-ID` is the identity of one attempt, and a new direct upload or retry must generate a new ID; resubmitting another request with a reused old ID returns 409 and does not overwrite the original receipt. Chunk staging may reuse the same UUID/upload ID, and in the completion stage only one worker is allowed to obtain the submission right.
