[简体中文](REAL-ACCOUNT-TEST.md) | [English](REAL-ACCOUNT-TEST.en.md)

# Real drive account testing and community feedback

Drive login, credential storage, and renewal are handled by OpenList; cloud2api only onboards existing storage and exposes file operations via API Keys. For the login wizard and platform prerequisites, see [the OpenList patch](../openlist-patch/README.en.md) and [the platform matrix](../openlist-patch/docs/PLATFORMS.en.md). Local storage and the example adapter can verify the flow, but cannot prove the login, renewal, and rate-limiting behavior of real drives.

Aliyun Drive, Baidu Netdisk, 123 Cloud Drive, and OneDrive have received user reports of successful login; Google Drive, as well as different account types, long-term renewal, and failure recovery, still need additional testing. Please record "login succeeded", "read/write succeeded", and "renewal succeeded" separately, and do not extend one of them into overall verification.

## Preparation

1. Use your own independent test account, storage, and dedicated directory, do not operate on other people's files, and do not simulate credential expiry on storage that is in use.
2. Deploy according to [the setup instructions](../README.en.md#set-it-up); use your own configuration for the service address and admin credentials, and do not write credentials into the test report.
3. As an administrator, enter the OpenList wizard from "Add drive" in the portal; drivers without an adapter can be created in the OpenList native backend and then onboarded back in the portal.
4. Create a temporary API Key authorized only for the test drive and test directory, preferring the "General" permission group. Grant the corresponding permission explicitly only when you need to delete test files; do not bypass permission problems with "All".

The default method may depend on a third-party public authorization service. When a public client such as Google Drive is unavailable, record that limitation or use your own application according to the platform documentation, and do not submit the third-party service's tokens or full callback address.

## General test checklist

| Item | What to check |
| --- | --- |
| Login and save | The QR-code, token-paste, or account-password flow succeeds; the result page returns a storage identifier; the OpenList driver initializes successfully |
| Return and onboarding | After returning to the portal and refreshing, it can be onboarded; the mapping points to the correct storage, and onboarding does not automatically widen existing Key authorization |
| Authorization discovery | `/v1/me` shows only the test Key's drives, root directories, and permissions |
| List directory | The authorized directory is readable; paths outside the root directory and unauthorized drives are rejected |
| Small-file read/write | The upload has a complete confirmation receipt; read-back bytes match the local ones; the official client can also see the test file |
| Workbench navigation | When entering a subdirectory, going back up, or switching drives, the upload target and path navigation stay consistent; a failed directory load does not show the previous directory's statistics |
| Upload and share | The upload does not become public automatically; after explicitly generating a share link, verify expiry and permission behavior |
| Restart recovery | Only after restarting an independent test instance, check whether the mapping, Key authorization, and storage identity are retained |
| Natural renewal | After the access token expires naturally, access it again, and record the driver's renewal result and the observed duration separately |
| Expiry and re-authorization | Verify only on disposable, independent test storage; after re-authorization, the original mapping, root directory, and Key scope remain unchanged |
| Cleanup | Delete only the files and test storage created this time, and revoke the test Key when finished |

For file-operation examples, see [README](../README.en.md#available-apis). The basis for an upload completing is a valid receipt and results such as size, CRC-64/XZ, and SHA-256, not send progress reaching 100%, an HTTP 200, or the appearance of file metadata.

## Login failure paths

Perform only the applicable items against independent test storage, and state the reason for items that are not applicable or were not tested.

- Authorization cancelled, denied, QR code expired, and refresh.
- A malformed token and a well-formed but invalid token; new-storage cleanup after a failed save.
- Duplicate callbacks, repeatedly clicking save, and conflicts with an existing mount path.
- During re-authorization, when the storage has been modified by another administrator, refusing to overwrite their configuration.
- Whether the original configuration is retained or restored after a failed re-authorization. Do not repeatedly enter wrong passwords and trigger real-account risk controls.
- Error pages and logs must not contain passwords, access/refresh tokens, client secrets, authorization codes, or full callbacks. When checking, avoid writing secrets into the command line, shell history, or public attachments.

## Platform focus

| Platform | Additional attention |
| --- | --- |
| Baidu Netdisk | Record public application and your own application separately; observe long-term renewal; large-file temporary space and rate limiting |
| Aliyun Drive Open | Online QR-code login, natural renewal, re-authorization, and behavior when the authorization service is unavailable |
| 123 Cloud Drive | Account-password verification and risk controls; do not confuse it with the `123 Open` third-party OAuth flow |
| OneDrive | Record personal and work/school accounts separately; configure special cloud regions and SharePoint according to the driver documentation |
| Google Drive | Record the public client and your own application separately; test personal Drive and shared drives separately |
| Other OpenList drivers | Connect in the native backend first, then test onboarding and restricted file operations; upstream support does not mean this project has verified it |

## Community testing and pull requests

You are welcome to help verify login items that OpenList supports but cloud2api has not yet tested thoroughly. When submitting a sanitized test report or a PR, update the corresponding record in [the platform matrix](../openlist-patch/docs/PLATFORMS.en.md); new adapters follow [the contribution guide](../openlist-patch/docs/CONTRIBUTING-adapters.en.md) and add failure-path regressions that do not depend on real credentials.

```text
Date:
Version (OpenList / connect / public cloud2api version):
Platform / driver / account type:
Login method (QR code / paste token / account password / own application / native form):
Login and onboarding: pass / fail / not tested
Restricted directory read/write and upload confirmation: pass / fail / not tested
Re-authorization / renewal: pass / fail / not tested
Cancel / expiry / save failure / recovery: pass / fail / not tested
Problem and reproduction steps (without real credentials, private paths, or file content):
```

Do not submit real databases, deployment configuration, raw requests/responses, full logs, or screenshots containing authorization QR codes. Even if tokens are redacted, also review the URLs, file names, accounts, paths, and other areas of the screenshot. A suspected credential leak should be revoked or rotated first, and the original text should not be pasted into a public Issue.
