[简体中文](API.md) | [English](API.en.md)

# connect 模块接口

带补丁的 OpenList 在自己的 HTTP 服务上增加以下接口。约定与 OpenList 一致：HTTP 状态恒为 200，响应体为 `{code, message, data}`，`code` 为 200 表示成功；失败时 `message` 是可直接展示的中文说明，`data.error_code` 是下表中的机器可读代码。

管理接口挂在 `/api/admin/connect/` 下，经过 OpenList 自己的 `Auth` 与 `AuthAdmin` 中间件：浏览器可以按 OpenList 原生方式使用管理会话（`localStorage.token` 作为 `Authorization` 头），也可以（connect 4 起）使用免登交接签发的 Cookie——该 Cookie `HttpOnly`、`SameSite=Strict`、30 分钟绝对过期、`Path` 仅限 `/api/admin/connect`，见下文[免登交接](#免登交接connect-4)。两种方式都只对 `/api/admin/connect/*` 生效，向导不把管理令牌写入浏览器存储。登录会话还绑定发起者的用户 ID 与 `Authorization` 指纹：同一用户的另一个令牌（例如 cloud2api 持有的永久令牌）看不到浏览器发起的会话。

| `error_code` | `code` | 含义 |
| --- | --- | --- |
| `invalid_input` | 400 | 参数缺失或格式错误 |
| `app_missing` | 400 | 该平台需要先配置应用 |
| `app_invalid` | 400 | 应用配置不完整或与模式不符 |
| `unsupported` | 400 | 适配器不支持该操作，例如重新授权 SharePoint 存储 |
| `state_mismatch` | 400 | OAuth state 不存在、已使用或不属于该会话 |
| `not_found` | 404 | 平台、会话或存储不存在（他人的会话也返回 404） |
| `conflict` | 409 | 并发修改、重复处理、状态不允许 |
| `expired` / `cancelled` | 410 | 会话或二维码已过期、已取消 |
| `limit` | 429 | 进行中的会话过多或请求过频 |
| `provider` / `network` | 502 | 网盘服务拒绝请求 / 无法连接 |
| `internal` | 500 | 内部错误 |

## 平台与应用配置

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

- 应用模式（`app_modes`）：`none`（不需要应用，如 123pan）、`public_app`（适配器内置的公开应用，驱动用它直接向网盘续期）、`online_service`（OpenList 在线授权服务，驱动经该服务续期）、`own_app`（管理员自己的开发者应用）。第一项是默认模式。
- 登录方式随模式变化：`method` 是默认模式的登录方式，`mode_methods` 列出其他模式的不同方式；`active_method` 是平台当前实际使用的方式（已保存配置的模式，或默认模式）。
- 没有保存应用配置时，默认模式为 `none`、`public_app` 或 `online_service`（地址为 `https://api.oplist.org`）的平台直接可用，`app.default` 为 `true`；默认模式为 `own_app` 的平台返回 `{"required": true, "configured": false, "mode": "", "client_secret_set": false, "problem": ""}`，需要先配置。首批平台都不需要先配置。
- 不需要应用的平台：`app` 为 `{"required": false, "configured": true, "mode": "none"}`。
- `public_app` 的凭据来自适配器，响应不含 `client_id`、`redirect_uri`、`revision` 等字段，`identity` 是打码后的 client_id。
- `callback_path` 已包含 OpenList 的 base path；自有应用的页面用 `location.origin + callback_path` 作为建议的回调地址。配置文件设置了 `site_url` 时，`suggested_redirect_uri` 是据此生成的完整地址。
- `problem` 非空表示已保存的配置当前无法使用（适配器校验失败的原因）。
- 应用密钥只写不读：响应只有 `client_secret_set`。自有应用的 `client_id` 不是密钥，管理员编辑时需要看到完整值。
- `module.handoff`（connect 4 起，布尔）表示本模块支持免登交接；较旧的模块没有该字段，cloud2api 视为不支持并回退为手动打开向导链接。
- cloud2api 只读取 `module` 的 `version/upstream_version/handoff` 与 `platforms[].id/name/driver/method`，用来判断 OpenList 是否带有本模块、哪些驱动可以重新授权、是否可发起免登。

### `POST /api/admin/connect/app/save`

请求 `{"platform", "mode", "client_id", "client_secret", "redirect_uri", "service_url"}`。`client_secret` 为空字符串时保留已保存的值（仅限同一模式、同一 client_id）。`mode` 为 `public_app` 时忽略其余字段，只保存模式。先经适配器 `CheckApp` 校验，通过后才写入；每次保存 `revision` 加一。返回该平台的 `app` 对象。

### `POST /api/admin/connect/app/delete`

请求 `{"platform"}`，返回删除后的 `app` 对象：有默认模式的平台回到默认。已发起的会话继续使用开始时的应用快照，已有存储保存的是自己的续期参数，都不受影响。

### `GET /api/admin/connect/mount/suggest?platform=baidu`

```json
{"mount_path": "/百度网盘"}
```

新建存储时建议的挂载路径：平台名称（去掉 `/` 和控制字符），被已有存储占用时依次尝试 `-2` 到 `-99`，都被占用时为空字符串。页面在「确认并保存」一屏的挂载路径为空时填入它。未知平台返回 `not_found`。

## 重新授权目标

### `GET /api/admin/connect/target?id=12`

```json
{"storage_id": 12, "mount_path": "/baidu", "driver": "BaiduNetdisk", "status": "work",
 "disabled": false, "modified": "2026-10-14T08:00:00Z",
 "platforms": [{"id": "baidu", "name": "百度网盘", "problem": ""}],
 "edit_path": "/@manage/storages/edit/12"}
```

`platforms` 是驱动与该存储一致的适配器；`problem` 说明为什么暂时不能重新授权（未配置应用、存储已停用、适配器不支持该存储的配置等）。为空数组时页面引导到 `edit_path` 使用 OpenList 原生表单。

## 登录会话

会话只在 OpenList 进程内存中，重启即失效。阶段（`phase`）：

| 阶段 | 说明 |
| --- | --- |
| `pending` | 等待用户授权、扫码或提交表单 |
| `authorized` | 凭据已在服务端暂存，等待选择挂载点并保存 |
| `saving` | 配置适配层正在写入 |
| `saved` / `save_failed` | 已保存 / 写入失败（见 `result`）；两种情况凭据都已清除 |
| `failed` | 登录本身失败（网盘拒绝、回调错误等） |
| `cancelled` / `expired` | 已取消 / 已过期；凭据已清除 |

时限：`pending` 10 分钟；`authorized` 后 15 分钟内保存；结束的会话结果保留 30 分钟供重复查询。每个管理员最多 5 个进行中的会话。

### `POST /api/admin/connect/session/start`

请求 `{"platform": "baidu"}`，重新授权时加 `"reauth_storage_id": 12`。返回会话视图（View）：

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

- 上例是百度在自有应用模式下的会话。`method` 是本会话的登录方式，由会话开始时的应用模式决定（见 `mode_methods`）：没有保存应用配置时使用平台默认模式（百度默认是公开应用扫码）；默认模式为 `own_app` 且没有配置时返回 `app_missing`。
- `qr`（扫码）：`{"status": "waiting|scanned|confirmed|cancelled|expired|network_error", "image_url", "content", "expires_at", "message", "can_refresh"}`。`image_url` 是网盘提供的二维码图片（页面以 `referrerpolicy="no-referrer"` 加载）；没有图片时页面以文本显示 `content`，首批的扫码适配器（阿里云盘、百度网盘公开应用）都提供图片。`message` 起初是二维码自带的提示（百度：「也可以在电脑浏览器打开 https://openapi.baidu.com/device 输入代码 …」），网盘返回的状态说明会替换它，网络错误恢复后回到该提示，二维码过期后清空。
- `form`（表单）：字段数组 `{key, label, type, required, secret, default, options, help, create_only}`；重新授权时不含 `create_only` 字段。
- `form_label`：表单登录的名称，例如「粘贴令牌」；为空时页面显示「账号密码」。
- `guide`（部分表单登录）：`{"url", "label", "steps": [...]}`，提交前要先打开的外部页面及操作步骤。百度、OneDrive、Google Drive 的「粘贴令牌」指向 OpenList 在线授权页，百度和 OneDrive 的地址带有预选该网盘选项的 `#` 参数，Google 不带（保留该页的 Google 使用说明弹窗）。
- `auth`（`authorized` 之后）：`{"credential_type", "app_mode", "app_identity", "renewal", "account", "fields": [...], "create_fields": [...]}`，只有字段名，没有值。
- `result`（保存之后）：见下文。

### `GET /api/admin/connect/session/get?id=…`

返回当前视图。扫码会话在这里向网盘查询状态，服务端每个会话至多每 2 秒查询一次，与页面轮询频率无关；网络错误显示为 `network_error` 并继续轮询，直到二维码过期。

### `POST /api/admin/connect/session/submit`

请求 `{"id", "values": {…}}`：

- 表单登录：各字段的值。校验失败时会话保持 `pending`，可修改后重新提交。
- 粘贴令牌：`{"refresh_token": "…"}`。接受令牌本身、含 `refresh_token` 的 JSON，或 `#` 后为 base64 编码 JSON 的授权页地址；首尾空白会去掉，中间有空白、长度小于 16 或大于 8192 时返回 `invalid_input`。服务端不检验令牌本身是否有效：保存时由驱动初始化检验，失败时删除本次创建的存储。
- OAuth 回调地址无法打开时：`{"callback_url": "<浏览器地址栏中的完整地址>"}`，其中的 state 必须属于该会话。
- 应用的 `redirect_uri` 为 `oob`（仅百度）：`{"code": "<页面显示的授权码>"}`。

### `POST /api/admin/connect/session/refresh`

请求 `{"id"}`，扫码会话重新生成二维码，每个会话最多 5 次。

### `POST /api/admin/connect/session/cancel`

请求 `{"id"}`。`pending` 与 `authorized` 会话转为 `cancelled` 并清除凭据；`saving` 返回 `conflict`；已结束的会话原样返回。

### `POST /api/admin/connect/session/save`

请求 `{"id", "mount_path", "root", "remark"}`；重新授权忽略后三项。同一会话只执行一次：重复或并发的请求等待并返回同一次结果。`result`：

```json
{
  "ok": false, "mode": "create", "storage_id": 13, "mount_path": "/baidu", "status": "",
  "message": "驱动初始化失败，已删除本次创建的存储 #13",
  "error": "failed init storage: …", "retryable": false, "credentials_written": true,
  "cleanup": {"attempted": true, "deleted": true, "storage_id": 13, "error": ""},
  "restore": null
}
```

- `retryable: true`：没有写入任何内容（挂载路径已被占用、路径格式错误等），会话保持 `authorized`，修改后可再次保存。
- 新建失败：`cleanup` 报告是否删除了**本次**创建的存储（按返回的存储 ID 并核对挂载路径与驱动，删除后重新读取确认）。
- 重新授权失败：`restore` 报告是否恢复了原配置（`restored` 只在重新读取确认原设置回到原位且新凭据已移除时为 true）以及恢复后的状态。`credentials_written` 说明新凭据是否曾写入存储。

## OAuth 回调

### `GET /api/connect/oauth/callback`

网盘授权完成后浏览器跳转到这里（应用配置中登记的回调地址）。不经过 OpenList 登录：由一次性的 state 关联会话，PKCE（OneDrive、Google）绑定授权码。服务端立即兑换授权码，然后返回一个不含凭据和授权码的结果页，提示回到原页面继续；state 未知或已使用时返回 400 页面。

授权码会出现在 OpenList 访问日志的请求地址里。它只能使用一次、在回调时立即兑换，兑换还需要服务端保存的应用密钥（及 PKCE verifier），日志中的授权码无法再被使用；需要完全避免时可在 OpenList 配置的日志过滤中排除该路径。

## 免登交接（connect 4）

管理员可以从 cloud2api 门户直接打开登录向导，而不必先手动登录 OpenList。流程：cloud2api 后端调用 `handoff/start` 取得一次性票据，向导地址以 URL fragment 携带票据与目标；页面读取后**立即清除 fragment**，用票据换取仅限 connect 管理接口的 Cookie。全程不经过跳转链接传递管理令牌，浏览器存储中不出现 OpenList 管理密码、完整管理 JWT 或长期管理令牌；原生 `/@manage` 后台不在免登范围内。本节接口已通过组件级测试、配对验收与最终真实浏览器核验（免登进入、取消、退出吊销 Cookie、跨站回跳；GUI 上传与原生弹窗点击流不在核验范围内），已随 2026-10-15 的本地部署上线。

### `POST /api/admin/connect/handoff/start`

经过 `Auth` 与 `AuthAdmin`，由 cloud2api 服务端（持有管理令牌）调用，浏览器不直接调用。请求体为空 JSON `{}`。响应 `data`：

```json
{"code": "<32 字节 base64url>", "expires_in": 60}
```

- `code` 是随机 32 字节的 base64url 编码（无填充），单次有效，60 秒内过期；`expires_in` 是剩余秒数（1–60）。票据只保存在 OpenList 进程内存中，重启即失效。

### `POST /api/connect/handoff/exchange`

公开接口，不要求登录。请求携带 `{"code": "<票据>"}`：票据有效时签发 Cookie（`Path=/api/admin/connect`、30 分钟绝对过期、`HttpOnly`、`SameSite=Strict`；值为仅限本模块的会话令牌，不是管理 JWT）。票据已使用、过期或格式错误时按上表返回错误，且不签发 Cookie。页面在交换成功后打开 fragment 中携带的目标；该 Cookie 只用于访问 `/api/admin/connect/*`。会话查询与注销：

| 接口 | 说明 |
| --- | --- |
| `GET /api/admin/connect/auth/me` | 返回当前免登会话的用户与能力（whoami） |
| `POST /api/admin/connect/auth/logout` | 注销免登会话并清除 Cookie |

向导地址的 fragment 形如 `#handoff=<票据>&target=/new/goapi`，`target` 只接受 `/`、`/new/<platform>`、`/app/<platform>`、`/reauth/<storage_id>`；cloud2api 侧在发起前已按 `platforms` 列表校验目标，页面侧同样严格校验（`/new/`、`/app/` 后的平台必须存在，`/reauth/` 后必须是正整数）。fragment 在页面加载时立即清除，不出现在历史记录之外的位置；票据本身一次性，重复打开旧地址只会得到失效提示。

## 页面

`GET /@connect/` 是最小登录配置页面，只包含 `index.html`、`app.js`、`app.css`，不含文件预览、画廊、影音播放和 Office 预览代码，也不加载外部脚本。页面路由：

| 地址 | 用途 |
| --- | --- |
| `/@connect/` | 平台列表与应用配置入口 |
| `/@connect/#/app/<platform>` | 应用配置 |
| `/@connect/#/new/<platform>` | 新建存储 |
| `/@connect/#/reauth/<storage_id>` | 重新授权（cloud2api 的入口） |
| `/@connect/#/s/<session_id>` | 回到进行中的会话（回调结果页链接） |

任一地址都可以带查询参数 `return`，表示完成后要回到的 cloud2api 页面，例如 `/@connect/?return=http%3A%2F%2F127.0.0.1%3A8080%2Fadmin%2Fconnect-return%3Fstorage_id%3D%7Bstorage_id%7D#/new/baidu`。页面只接受长度不超过 2048、能解析、协议为 http/https、不含用户名密码，且主机名与本页相同或为本机回环地址（`localhost`、`127.x.x.x`、`[::1]`，门户用 127.0.0.1 打开而本页用局域网地址打开时）的地址，其他值忽略。保存成功后的主按钮是「在 cloud2api 中查看」，地址中的 `{storage_id}` 替换为本次保存的存储 ID；保存失败页和首页显示「返回 cloud2api」，未取得存储 ID 时（保存失败、取消、票据过期等）返回不带查询参数的中转页地址，由它跳回门户列表 `/admin/#/drives`。cloud2api 门户的所有登录向导链接都带这个参数。

cloud2api 传入的 `return` 指向它自己的回跳中转页 `GET /admin/connect-return`，而不是直接的门户 hash 地址：OpenList 常以局域网地址访问而门户在回环地址上，直接跳回门户是跨站点导航，门户的 `SameSite=Strict` 会话 Cookie 不会被携带。中转页在门户源上先加载一个静态页面（CSP 限定 self、`no-store`、不下发 Cookie），再由脚本做同源跳转：带 `storage_id` 时跳到 `/admin/#/drives?connected=<存储 ID>`，不带查询参数时跳到 `/admin/#/drives`，这次同源导航就能带上会话；SameSite 策略不因此放宽。该页无需认证、也不是登录入口；带 `storage_id` 参数时只接受唯一一个正整数，空值、格式不符、重复或多余的查询参数返回 400。

新建时，授权完成后是一屏「确认并保存」：凭据摘要、挂载路径（预填 `mount/suggest` 的建议）、根目录和备注；重新授权只确认目标存储后保存。
