[简体中文](CONTRIBUTING-adapters.md) | [English](CONTRIBUTING-adapters.en.md)

# Adding a Platform Adapter

The login SDK's adapters are built with the code and registered at compile time; there is no dynamic plugin loading. Adding a platform usually requires submitting just three things: the adapter package and tests under `src/pkg/loginsdk/adapters/<name>/`, one `Register` line in [`adapters/all/all.go`](../src/pkg/loginsdk/adapters/all/all.go), and the description in [PLATFORMS.md](PLATFORMS.en.md). No other platform, OpenList driver, configuration adaptation layer (`internal/loginbridge`), HTTP layer (`server/connect`) or cloud2api needs to be modified.

Sessions, state and PKCE, QR-code polling cadence, credential staging, saving and failure recovery, and page steps are all handled uniformly by the SDK. An adapter only describes the platform and converts the login result into driver fields.

## 1. Choose the login method

Start by copying a package from [example](../src/pkg/loginsdk/adapters/example/example.go), and implement one of the following three interfaces for the platform (definitions in [sdk.go](../src/pkg/loginsdk/sdk.go)):

| `Meta.Method` | Interface | Adapter provides | Reference |
| --- | --- | --- | --- |
| `oauth` | `OAuthAdapter` | `AuthorizeURL` (the SDK passes in state and PKCE), `Exchange` (exchanges the authorization code) | `baidu`, `onedrive`, `googledrive` |
| `qrcode` | `QRAdapter` | `StartQR` (QR-code image URL or content), `PollQR` (maps to the six QR-scan states) | `aliyunopen` |
| `form` | `FormAdapter` | `Submit` (validates the form and produces the authentication config) | `pan123`, `example` |

Every adapter implements `Meta()` and `CheckApp(app)`. When reauthorization needs to be refused for certain storages (for example, other OneDrive regions), also implement `ReauthChecker`.

Different app modes can use different login methods: `Meta.Method` is the method of the first entry in `AppModes` (the default mode), and `Meta.ModeMethods` specifies a separate method for each other mode; the adapter must implement all the interfaces used. For example, Baidu's default is QR code, paste token uses `form`, and own app uses `oauth`; OneDrive and Google Drive default to `form` (paste token), and own app uses `oauth`. There are two further optional interfaces:

| Interface | Purpose | Reference |
| --- | --- | --- |
| `PublicAppProvider` | Must be implemented when listing the `public_app` mode; returns the client_id and client_secret of the public app used | `baidu` |
| `FormGuider` | When the form requires the user to first retrieve a token on another page, returns the button that opens that page and the step instructions | `baidu`, `onedrive`, `googledrive` |

## 2. Filling in Meta

- `ID`: lowercase letters, digits, `-` or `_`, at most 32 characters, never changed after release. The page address and the app configuration are keyed by it.
- `Driver`: must exactly match the name under which the upstream driver is registered (`config.Name` in `drivers/<name>/meta.go`). Reauthorization matches existing storages by it.
- `AppModes`: `own_app` (own app), `online_service` (OpenList online authorization service, default `https://api.oplist.org`), `public_app` (an app published publicly by upstream tooling, see `PublicAppLabel`), or the single value `none` (no app needed; `none` cannot be combined with other modes). The first entry is the default mode: when it does not require the administrator to fill in parameters (`online_service`, `public_app`, `none`), login can be performed directly even with no saved app configuration. Other modes are explicitly chosen by the administrator in the app configuration and cannot be inferred from whether a field is empty.
- `ModeMethods`, `FormLabel`: see the previous section; `FormLabel` is the name of the form method on the page, for example "Paste token"; when left empty, "Account and password" is shown.
- `Scopes`, `PKCE`, `AllowOOB`: the page displays the requested permission scopes. PKCE must be enabled when the provider supports it. `AllowOOB` indicates that the callback address may be set to `oob` and the user pastes the authorization code.
- `FormFields`: the fields of the form method; values with `Secret: true` are scrubbed from all messages. Fields with `CreateOnly: true` are shown only on creation, and the adapter must not require them on reauthorization.
- `RootField` and `RootDefault`: the driver's root-directory field, whose value is `root_folder_path` or `root_folder_id`. The default should match the driver's `DefaultRoot`.
- `ReauthFields`: the addition keys that reauthorization is allowed to replace. All other storage settings are preserved. List only credentials and the renewal parameters tied to them.
- `Notes`: the limitations and prerequisites shown on the page (in Chinese). `DocsURL` points to the upstream driver documentation.
- `Verified`: real-account verification records, for example `2026-10-20 personal account`. When empty, the page shows "Not verified with a real account".

`loginsdk.Validate` checks these rules at registration time; a non-compliant adapter fails at startup.

## 3. Generating the authentication config

`Exchange`, QR-code confirmation or `Submit` returns a `*loginsdk.AuthConfig`. Before saving, the SDK validates it with `ValidateAuth`:

- `Driver` equals `Meta.Driver`, and `AppMode` equals this session's app mode.
- `Fields`: fields written both on creation and on reauthorization. All keys must appear in `ReauthFields`. Key names are the JSON keys of the driver's Addition; fields without a json tag use the Go field name, for example `AccessToken`.
- `CreateFields`: connection settings written only on creation; they must not contain the root-directory field or any key in `ReauthFields`.
- `Secrets`: every secret value produced or used by this login (token, password, authorization code). The SDK and the configuration adaptation layer use it to scrub credentials from responses and logs.
- `CredentialType`, `AppIdentity`, `Renewal`, `Account`: a redacted summary shown on the page. `Renewal` explains in one sentence how the driver will renew afterwards. `Account` is an optional account name; leave it empty when the query fails, and login must not fail because of that.

Authentication fields must be consistent with the driver's actual renewal method. For example, when using an own app, write `use_online_api=false` and the same set of app parameters, and do not keep the driver's default online-service mode.

## 4. Implementation constraints

- **Do not refresh tokens.** Renewal belongs to the driver. The adapter only exchanges the authorization code, or only stages the account and password for the driver to initialize and verify on save. Actively refreshing could rotate the token and cause the driver and the adapter to refresh the same credential at the same time.
- **Errors use `loginsdk.Errorf` and error codes** (`provider`, `network`, `invalid_input`, `app_invalid`, etc.), with messages in Chinese. Error text returned by the provider is first scrubbed of secret values with `loginsdk.Scrub` before being placed in the message.
- **Network requests use `loginsdk.Do` or `DoJSON`.** They carry timeouts and response size limits, and on connection failure report only the host name, without the query string.
- **Define provider addresses as package-level variables**, so tests can point them at an `httptest` server.
- **Do not write logs**, and do not include tokens, passwords, authorization codes or app secrets in errors.
- **QR code**: return the provider's https image address (`ImageURL`), or text that the page must draw (`Content`). The provider's session identifier goes in `Private`, which is not sent to the browser. Fill in both `ExpiresAt` and `PollEvery`; the SDK polls at most once every 2 seconds per session, and when the provider requires a longer interval (for example the `interval` of device-code authorization) use the provider's value. `Message` is the hint shown while waiting for the scan, for example an alternative input method when not scanning; a `Message` returned by `PollQR` temporarily replaces it, and when an empty message is returned (and after a network error recovers) the page shows it again, and it is no longer shown after the QR code expires.
- **Paste token**: use `loginsdk.ParsePastedToken` to parse what the user pastes; it accepts the token itself, JSON with a `refresh_token`, and the api.oplist.org callback URL format. The token is initialized and verified by the driver on save; the adapter should not refresh it in order to validate it.

## 5. Testing

Adapter tests follow the same pattern as [baidu](../src/pkg/loginsdk/adapters/baidu/adapter_test.go) and [aliyunopen](../src/pkg/loginsdk/adapters/aliyunopen/adapter_test.go), using `httptest` to simulate the provider, and must cover at least:

- `loginsdk.Validate(New())` passes.
- `CheckApp`: valid and invalid configurations for each app mode.
- The parameters of the authorization URL or the QR-code request, including state, PKCE challenge and permission scopes.
- `Fields`, `CreateFields`, `Secrets` and `AppMode` after a successful exchange.
- Provider refusal, an error body inside HTTP 200, a missing token, and network errors; no secret value appears in the error messages.
- QR-code methods must also cover the mapping of every provider state to the six QR-scan states.

Then run the full test suite (including `-race`) and confirm that the `adapters/all` tests still pass:

```bash
OPENLIST_MIRROR=$HOME/src/openlist-upstream openlist-patch/scripts/test.sh
```

Adapters for the form method can follow `scripts/e2e.sh` and run through saving and failure recovery with a real driver.

## 6. Real-account verification and submission

Adapters without real-account verification may still be merged, but `Verified` must be left empty and the page will mark them "Not verified with a real account". Contributors who have an account should, after running the six platform acceptance tests in [PLATFORMS.md](PLATFORMS.en.md#verification-records), fill in the record table with:

- Test date, account type (personal, enterprise, membership tier, etc.).
- Driver name, authentication method, app mode.
- OpenList version and connect version (the `module` of `GET /api/admin/connect/platforms`).
- The result of each item, including at least one real renewal, and any limitations encountered.

Submission checklist:

- [ ] The new adapter package and tests, plus one registration line in `adapters/all/all.go`.
- [ ] No other platform, driver, `internal/loginbridge`, `server/connect` or cloud2api has been modified.
- [ ] `test.sh` passes in full.
- [ ] A platform description has been added to PLATFORMS.md, with verification records filled in or noted as unverified.

115 is not in the scope of first-wave maintenance; contributors with an account are welcome to follow this document to add an adapter. Please state the driver used: `115 Cloud` and `115 Open` are different drivers, and credentials cannot be mixed between them.
