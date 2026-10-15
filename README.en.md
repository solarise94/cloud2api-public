[简体中文](README.md) | [English](README.en.md)

<p align="center">
  <img src="internal/admin/web/assets/logo.png" alt="cloud2api project logo" width="200">
</p>

<h1 align="center">cloud2api</h1>

<p align="center">Turn your cloud drives into an API, or upload, download, and share from your browser.</p>

cloud2api uses OpenList to give different cloud drives the same file API. Manage files in your browser, or give a script or AI agent an API Key that grants access to specific drives and folders.

## What it does

- **Upload and share**: drop files into the page, then create an expiring download link when you want to share them.
- **Manage files**: browse folders, download, create folders, rename, delete, copy, and transfer files between drives.
- **Work with scripts and agents**: use one API across your drives instead of integrating each provider separately.
- **Control access**: limit each Key to selected drives, folders, and permissions, with General, Read-only, and All presets.
- **Remember your login**: file workspaces stay signed in for 30 days by default and can be bookmarked. Administrator sessions last 24 hours by default.
- **Connect another cloud2api**: choose API upstream when adding storage, then enter the other instance's address and Key.

OpenList handles drive login and connection settings; cloud2api provides the file workspace, API, and access controls. The login wizard includes Baidu Netdisk, Aliyun Drive, 123 Cloud Drive, OneDrive, and Google Drive. Other drivers can be added through OpenList's administration page. Google Drive has not yet been tested with a real account; see the [support list](openlist-patch/docs/PLATFORMS.en.md).

## Set it up

Use a Linux machine with Bash, Python 3, and working rootless Podman, including `slirp4netns`. Builds run in containers, so you do not need Go or Node.js on the host.

For a new installation:

```bash
git clone https://github.com/solarise94/cloud2api-public.git
cd cloud2api-public

# Keep any existing configuration.
if [ ! -e config.yaml ]; then
  install -m 600 config.production.example.yaml config.yaml
fi

PUBLISH_ADDR=127.0.0.1 \
OPENLIST_PUBLISH_ADDR=127.0.0.1 \
OPENLIST_PUBLIC_URL=http://127.0.0.1:5244 \
SEED_STORAGES=0 \
bash scripts/deploy.sh
```

The first build downloads dependencies and starts cloud2api and OpenList. These settings allow access from the local machine only. For access from another device, adjust the publish addresses and `OPENLIST_PUBLIC_URL`. Use HTTPS for public access.

| Page | Address |
| --- | --- |
| Home | `http://127.0.0.1:8080/` |
| File workspace | `http://127.0.0.1:8080/files/` |
| Administration | `http://127.0.0.1:8080/admin/` |
| OpenList administration | `http://127.0.0.1:5244/@manage` |

The administrator password is saved in `cloud2api-data/admin_password`; the OpenList password is in `cloud2api-data/openlist_admin_password`. Before upgrading, back up `config.yaml`, `cloud2api-data/`, and `openlist-data/`, and keep your existing configuration and data.

## Use it in your browser

1. Open the administration page and sign in with the administrator password.
2. Click “Add drive,” follow the login wizard, save the storage, then return to cloud2api to add it to your managed drives.
3. Create an API Key and select the drives and folders it can access. General works for most uses; choose Read-only for browsing and downloads. General does not include deletion.
4. Open the file workspace and sign in with that Key. A single-drive Key opens its drive directly; with multiple drives, choose one first.
5. Drop files into the upload area. To share a file, click “Get link” and choose an expiration of up to seven days.

Uploading does not automatically make a file public. You can bookmark a workspace, but the address alone does not let someone else access your files without a valid login.

## Available APIs

Scripts and agents authenticate with an API Key in the `apikey` header. `Authorization: Bearer <API Key>` also works. `{drive}` is a drive ID returned by `/v1/me`; file paths are relative to the root folder granted to that Key.

The main endpoints are:

| Method | Endpoint | What it does |
| --- | --- | --- |
| GET | `/v1/me` | Show available drives and permissions |
| GET | `/v1/drives/{drive}/list` | List a folder |
| GET | `/v1/drives/{drive}/stat` | Get file information |
| GET / PUT | `/v1/drives/{drive}/files` | Download / upload a file |
| POST | `/v1/drives/{drive}/mkdir` | Create a folder |
| POST | `/v1/drives/{drive}/delete` | Delete a file or folder |
| POST | `/v1/drives/{drive}/move` | Move or rename within a drive |
| POST | `/v1/transfers` | Copy files or move them between drives |
| GET | `/v1/jobs/{id}` | Check an asynchronous task |
| POST | `/v1/links` | Create an expiring share link |
| GET | `/l/{token}` | Download through a share link without a Key |

### Check which drives you can access

```bash
export C2A_BASE_URL=http://127.0.0.1:8080
read -r -s -p 'API Key: ' C2A_API_KEY
printf '\n'
export C2A_API_KEY

curl --fail-with-body \
  -H "apikey: $C2A_API_KEY" \
  "$C2A_BASE_URL/v1/me"
```

### List, upload, and download

Replace `your-drive-id` with a drive ID from the previous response:

```bash
DRIVE_ID=your-drive-id

# List the root folder.
curl --fail-with-body -G \
  -H "apikey: $C2A_API_KEY" \
  --data-urlencode 'path=/' \
  "$C2A_BASE_URL/v1/drives/$DRIVE_ID/list"

# Upload a local file as /example.txt on the drive.
curl --fail-with-body -X PUT \
  -H "apikey: $C2A_API_KEY" \
  -H 'Content-Type: application/octet-stream' \
  --data-binary @./example.txt \
  "$C2A_BASE_URL/v1/drives/$DRIVE_ID/files?path=/example.txt"

# Download it to a local file.
curl --fail-with-body \
  -H "apikey: $C2A_API_KEY" \
  -o downloaded.txt \
  "$C2A_BASE_URL/v1/drives/$DRIVE_ID/files?path=/example.txt"
```

Check the API's confirmation before treating an upload as complete. If an operation returns an asynchronous task ID, check `/v1/jobs/{id}`; “accepted” does not mean “finished.”

### Connect an AI agent

Configure your agent with your own service address and API Key, then provide whichever of these fits your tool:

- `/custom/agent-instructions.txt`: agent instructions.
- `/custom/SKILL.md`: Skill documentation.
- `/apidocumentation/openapi.json`: OpenAPI for the main endpoints.
- `/custom/c2a.py`: a Python command-line tool.

## More details

- [File workspace](docs/FILE-WORKSPACE.en.md): uploads, sharing, folders, and saved logins.
- [Permissions and relay](docs/API-KEYS-AND-RELAY.en.md): Key settings, Gokapi-compatible uploads, and the separate `/relay/` upload page.
- [API development guide](docs/API-DEVELOPMENT.en.md): request parameters, response formats, advanced endpoints, and compatibility APIs.
- [OpenList patch guide](openlist-patch/README.en.md): the login wizard, builds, and adapter development.
