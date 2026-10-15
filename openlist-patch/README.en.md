[简体中文](README.md) | [English](README.en.md)

# openlist-patch: OpenList with login-adapter patches

The cloud2api deployment consists of two services: the OpenList built from this directory, and cloud2api. The patches here add a connect module to a pinned version of OpenList, for cloud-drive login, authorization and storage configuration; from connect 4 they also include a `goapi` upstream adapter (together with the new GoAPI storage driver) and an administrator passwordless-handoff interface. The file engine, drivers and renewal remain the responsibility of upstream OpenList. cloud2api only links to the patch pages; it never receives credentials or writes storage config. The current interface contract is in the [API documentation](docs/API.en.md), and the adapter method is in the [contributing guide](docs/CONTRIBUTING-adapters.en.md).

- Upstream version: OpenList v4.2.6 (commit `2bdf16d`), front-end artifacts v4.2.6, pinned in [UPSTREAM](UPSTREAM) and verified with sha256.
- License: OpenList uses AGPL-3.0; the patches and new code in this directory are released together with OpenList under AGPL-3.0.

## Contents

| Path | Contents |
| --- | --- |
| `UPSTREAM` | Upstream repository, version, commit, front-end artifact address and checksum, build image, connect version |
| `patches/0001-register-connect-module.patch` | The only upstream integration point: calls `connect.Register(g)` in `server/router.go` |
| `patches/0002-aliyundrive-open-redact-token-log.patch` | The upstream AliyundriveOpen driver prints the old and new refresh tokens in INFO logs; this patch removes the tokens from the logs |
| `patches/0003-hop-aware-relay-singleflight.patch` | Relay-loop deadlock prevention: the list/link/mkdir singleflight keys of `internal/op` carry the `X-GoAPI-Hop` hop count (`src/internal/op/relayflight.go`), so a relay sub-request no longer joins an ancestor flight that is blocking on itself, and a loop terminates as an ordinary error at the hop limit (see N6h); the cache key remains hop-free |
| `src/pkg/loginsdk/` | Login SDK: contract, session management, registry, HTTP utilities |
| `src/pkg/loginsdk/adapters/` | Platform adapters (Baidu, Aliyun, 123pan, OneDrive, Google Drive; from connect 4 including the API upstream `goapi`, plus the example registered only in example builds) |
| `src/drivers/goapi/` | The GoAPI storage driver added in connect 4: mounts a remote cloud2api `/v1` instance as a storage, and verifies the peer's `relay_hop_limit=4` before connecting |
| `src/pkg/goapiclient/` | GoAPI relay protocol client: capability verification, streaming download, upload-receipt verification by streamed bytes |
| `src/server/goapihop/` | Inbound validation and context propagation of the `X-GoAPI-Hop` hop-count header |
| `src/internal/loginbridge/` | Configuration adaptation layer: creates storages and reauthorises through OpenList's own storage operations, handles failure cleanup and recovery, and persists app configuration |
| `src/server/connect/` | HTTP interface (from connect 4 including passwordless handoff and GoAPI driver registration), OAuth callback, the minimal login configuration page `/@connect/` (only three files under `web/`) |
| `docs/API.md` | Interface contract of the connect module |
| `docs/PLATFORMS.md` | Authentication methods, drivers, app prerequisites and real-account verification status of the first platforms |
| `docs/CONTRIBUTING-adapters.md` | How the community adds platform adapters and the submission requirements |
| `dev/` | Local mock development server for the pages (not included in the image) |
| `scripts/` | Prepare the source tree, test, build the image, check the page artifacts, end-to-end test |

Files under `src/` are only copied into the upstream source tree as new files; `prepare-tree.sh` refuses to overwrite any upstream file, and the only changes to upstream code are the three in `patches/`.

## Common commands

```bash
# Unit tests: gofmt, go vet, whole-program build, tests with and without the example adapter, -race
OPENLIST_MIRROR=$HOME/src/openlist-upstream openlist-patch/scripts/test.sh

# Build image localhost/openlist-connect:v4.2.6-connect.<CONNECT_VERSION> (and :latest)
openlist-patch/scripts/build-image.sh

# Check the login page artifacts (only three files allowed; no preview, gallery, media or Office components)
openlist-patch/scripts/check-bundle.sh

# End-to-end test: build the example image, start a one-off OpenList, run the full create, read/write, reauthorise and failure paths
openlist-patch/scripts/e2e.sh
```

When `OPENLIST_MIRROR` points to a local clone of the upstream repository, no network is accessed; when it is unset, the upstream is cloned from GitHub. Front-end artifacts are cached in `~/.cache/openlist-connect/`. Builds and tests run in podman containers, so no Go toolchain is needed on the host.

When `OPENLIST_IMAGE` is not specified, `scripts/deploy.sh` uses the image built from this directory, builds it automatically if it does not exist, and `OPENLIST_REBUILD=1` forces a rebuild.

## Version tracking

The image tag is `<upstream version>-connect.<CONNECT_VERSION>`. `openlist version` reports the upstream version and commit, and the image labels `openlist.upstream.*` and `openlist.connect.*` record the upstream version, the connect version and the git revision of the patch source. A running instance returns the same information in the `module` field of `GET /api/admin/connect/platforms`, which the cloud2api portal uses to display the login-module status.

`conf.Version` keeps the upstream `v4.2.6`: OpenList applies its database upgrade patches according to this version number, and changing it would make the upgrade logic misjudge.

## Upgrading upstream

1. Update the version, commit and front-end artifact checksum in `UPSTREAM`.
2. Run `test.sh`: patches that fail to apply, and changes in the semantics of upstream storage operations (the write-to-database timing of create, update and delete), all fail here. The tests of `internal/loginbridge` cover those semantics.
3. Run `e2e.sh`, then re-check the verified platforms against [PLATFORMS.md](docs/PLATFORMS.en.md).

## connect 4

The new capabilities and fixes of connect 4, whose behaviour contract is in [docs/API.md](docs/API.en.md):

- **API upstream adapter**: the adapter with platform ID `goapi`, together with the new GoAPI storage driver, exposes the cloud2api `/v1` layered interface of another goapi service as an ordinary storage; the upstream address and key are written only into that driver. It targets only a cloud2api `/v1` upstream and does not provide general GoAPI/Gokapi compatibility: the Gokapi flat upload API is not used as an upstream protocol; before connecting, the hop-chain protection capability is explicitly verified via `/v1/me` (the field `relay_hop_limit` must have the value 4; the driver rejects a failed check), old `/v1` instances are not automatically compatible, and the peer must be upgraded first if necessary.
- **Passwordless handoff**: `POST /api/admin/connect/handoff/start` issues a 60-second single-use 32-byte base64url ticket, and the public endpoint `POST /api/connect/handoff/exchange` exchanges it for a Cookie with a 30-minute absolute expiry, `HttpOnly`, `SameSite=Strict` and `Path=/api/admin/connect`; the wizard address carries the ticket in the URL fragment and clears it immediately. The wizard page strictly validates `target` (the platform in `/new/<platform>` and `/app/<platform>` must exist, and `/reauth/<storage_id>` must be a positive int64); the earlier defect of double-encoding the target in the fragment has been fixed. The `module.handoff` boolean of `GET /api/admin/connect/platforms` lets cloud2api negotiate the capability, and older modules fall back to manual login.
- **System namespace and virtual directories**: after a successful initialisation probe the GoAPI driver generates a random 128-bit `system_ns` for the storage once and stores it with the storage (preserved on reauthorization; the wizard has no input for it, and there is no primary-database migration and no new credential). The virtual paths `/.c2a-trash` and `/.relay` map to storage-private ordinary directories `.goapi-system-<ns>-<storage ID>-trash` / `-relay` under the configured root, so the gateway's trash/restore and relay upload work as usual through them; do not manually modify, copy or empty these directories on the cloud-drive side (doing so would leave the virtual aliases dangling). Addressing boundaries: the physical aliases are not directly addressable in the virtual view; the virtual `/.c2a-trash` and `/.relay` are by design for internal gateway use and the native admin console; the reserved-path guard for external access through cloud2api `/v1` is unchanged.
- **Bug fixes**: two defects, the handling of OpenList `Range.Length=-1` for full downloads and the handling of upload Content-Length, have been fixed and are covered by tests. The driver also adds a **native Getter**: `op.Get` on a driver without a Getter returns NotFound when the root cache is stale, and the driver's own Getter fixes the root-directory make-directory mapping defect with it, covered by RED/GREEN integration tests over real MakeDir/Get/List cache warm-up/Put/Move/restore (including the `/data` root). The patched image has been rebuilt.

connect 4 has been validated in an isolated environment for component tests, build, race detection and multi-hop pairing behaviour, including the hop limit, trash restore, upload receipt and byte consistency. Passwordless handoff and the return-to-portal flow also have browser checks; this does not claim that real cloud-drive GUI uploads, third-party general GoAPI/Gokapi, all account types or long-term renewal have been verified. The Google Drive real account still awaits testing. Reproducible entry points are the test commands above, and the community testing scope is in the [real-account testing guide](../docs/REAL-ACCOUNT-TEST.en.md). The record in the next section is only the historical validation scope of connect 3.

## connect 3 validation record (2026-10-14, most recent completed validation)

This round only updates the user-tested markers for Baidu and OneDrive; Aliyun, Baidu, 123 and OneDrive have received feedback of successful real-account login, while Google keeps the awaiting-test marker. A successful login does not mean long-term renewal or all error paths have been verified.

- `test.sh`: gofmt, vet, whole-program build, production/example registration tests and race all pass.
- `e2e.sh`: 67 items pass, including create, file read/write, failure cleanup, reauthorise recovery, default login mode and no-credential-leak, using a one-off `v4.2.6-connect.3-example` instance, without touching any real storage.
- Contributions of untested account types and platforms are welcome; submit a redacted report or an adapter PR following the [real-account testing and PR guide](../docs/REAL-ACCOUNT-TEST.en.md#community-testing-and-pull-requests).
