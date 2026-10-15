[简体中文](PLATFORMS.md) | [English](PLATFORMS.en.md)

# 首批平台

首批五个平台各有一种不需要任何配置的默认登录方式，管理员也可以在「配置应用」中改用自有开发者应用。适配器只负责取得凭据，并把它和匹配的续期参数填进对应驱动的字段；续期由驱动完成，适配器不调用任何刷新接口，避免与驱动同时刷新同一凭据。其他平台和其他账户类型继续使用 OpenList 原生存储表单。connect 4 另增一个非网盘平台「API 上游（`goapi`）」，见 [下文专节](#api-上游goapiconnect-4)。

**验证状态（截至 2026-10-14）**：用户已反馈阿里云盘、百度网盘、123云盘和 OneDrive 登录实测通过，Google Drive 尚未实测。这里的「已验证」只表示已收到真实账号登录成功的反馈，不代表所有账号类型、重新授权、失败恢复或长期续期均已验收；未测的项仍留空。欢迎使用者测试 Google Drive、自有应用登录、不同账号类型，以及 OpenList 已支持而 cloud2api 尚未充分测试的其他驱动，并提交脱敏结果或 PR，见 [真实账号测试与贡献](../../docs/REAL-ACCOUNT-TEST.md#社区测试与-pr)。

| 平台 | 适配器 | 驱动 | 默认方式（无需配置） | 可选方式 | 续期 | 重新授权替换的字段 |
| --- | --- | --- | --- | --- | --- | --- |
| 百度网盘 | `baidu` | `BaiduNetdisk` | 扫码：百度设备码授权，使用公开应用 | 粘贴令牌（OpenList 在线授权服务）；浏览器授权（自有应用，可用 `oob`） | 公开应用和自有应用由驱动直接向百度续期（`use_online_api=false`）；粘贴令牌经在线授权服务续期 | `refresh_token` `AccessToken` `use_online_api` `api_url_address` `client_id` `client_secret` |
| 阿里云盘 | `aliyun` | `AliyundriveOpen` | 扫码：OpenList 在线授权服务（`https://api.oplist.org`） | 扫码（自有开放平台应用） | 在线服务模式经 `api_url_address` 续期；自有应用直接续期 | `refresh_token` `AccessToken` `use_online_api` `api_url_address` `alipan_type` `client_id` `client_secret` |
| 123云盘 | `123pan` | `123Pan` | 账号密码 | — | 令牌失效时驱动用账号密码重新登录 | `username` `password` `AccessToken` |
| OneDrive | `onedrive` | `Onedrive` | 粘贴令牌：OpenList 在线授权服务 | 浏览器授权 + PKCE（自有 Microsoft Entra 应用，global 区域） | 粘贴令牌经在线授权服务续期；自有应用直接续期 | `refresh_token` `use_online_api` `api_url_address` `client_id` `client_secret` `redirect_uri` |
| Google Drive | `googledrive` | `GoogleDrive` | 粘贴令牌：OpenList 在线授权服务 | 浏览器授权 + PKCE（自有 Google Cloud OAuth 客户端） | 粘贴令牌经在线授权服务续期；自有应用直接续期 | `refresh_token` `use_online_api` `api_url_address` `client_id` `client_secret` |

没有保存应用配置时，平台使用默认方式，页面标注「默认」；在「配置应用」中保存其他方式后改用所选方式，「恢复默认」删除保存的配置。适配器按所选方式写入续期参数，不根据字段是否为空推断令牌来源。重新授权只替换上表中的字段，挂载点、根目录、驱动类型等其他设置保持原样。

「粘贴令牌」与 OpenList 原生表单常用的取令牌方法相同：在 OpenList 授权页 api.oplist.org 上用 OpenList 的应用登录，页面显示刷新令牌后复制回来。OpenList 驱动续期时固定使用该服务的某个应用类型（百度 `baiduyun_go`、OneDrive `onedrive_pr`、Google `googleui_go`），因此授权页上必须选对应的选项；登录页的「打开 OpenList 授权页」按钮会尽量预先选好。令牌在保存存储时由驱动初始化验证，无效时按新建失败处理并清理。

## 各平台说明

### 百度网盘

- 默认「扫码登录」使用百度的设备码授权：登录页显示百度生成的二维码，用百度网盘 App 扫码并确认；也可以在电脑浏览器打开 `https://openapi.baidu.com/device`，输入页面显示的代码。
- 扫码所用的是 api.oplist.org「百度网盘 - 手动登录」预填的公开应用（AppKey `NqOMXF6XGhGRIGemsQ9nG0Na`）。这组 AppKey 和 SecretKey 会写入存储，驱动用它们直接向百度续期，不经过 api.oplist.org。它由 OpenList 授权工具公开发布，并非本项目所有；如果被撤销或限流，改用下面两种方式。
- 「粘贴令牌」：在 api.oplist.org 选「百度网盘 - 验证登录 (OAuth App)」并勾选「使用 OpenList 提供的参数」，取得刷新令牌后粘贴；驱动经在线授权服务续期。
- 「自有应用」：在 [百度网盘开放平台](https://pan.baidu.com/union) 创建应用，取得 AppKey（client_id）和 SecretKey（client_secret），并登记登录页面显示的回调地址 `…/api/connect/oauth/callback`；无法登记时把回调地址设为 `oob`，授权后把百度页面显示的授权码粘贴回登录页面。
- 新存储使用驱动的其他默认值（下载接口 `official` 等），需要调整时在 OpenList 原生表单中修改。

### 阿里云盘

- 只接入开放平台驱动 `AliyundriveOpen`。默认使用 OpenList 在线授权服务扫码，写入 `use_online_api=true` 和该服务的续期地址；自有应用模式写入 `use_online_api=false` 和应用参数。两种凭据不能互换。
- 二维码是阿里云盘提供的图片，页面直接显示。轮询区分等待、已扫码、已确认、手机上取消、过期和网络错误六种状态：网络错误时继续轮询；过期的二维码变暗，可以刷新；在手机上取消会结束本次会话，需要重新开始。
- 新存储挂载资源库（驱动默认 `drive_type=resource`），需要备份盘时在原生表单中修改。

### 123云盘

- 使用普通账号的 `123Pan` 驱动，不需要开发者应用。它与 `123 Open` 开放平台驱动是不同的适配，凭据不能混用。
- 登录页面不会自行调用 123 的登录接口，账号密码在保存存储时由驱动登录验证，以免重复登录触发风控。上游说明该驱动不再主动维护。

### OneDrive

- 默认「粘贴令牌」：登录页的「打开 OpenList 授权页」会预选「OneDrive - 个人版本 (OAuth App)」并勾选「使用 OpenList 提供的参数」；点「获取 Token」登录 Microsoft 账号，复制页面显示的刷新令牌粘贴回登录页。个人账号和工作或学校账号都选这一项，驱动续期时固定使用 `onedrive_pr`。工作或学校账号可能需要组织管理员同意。
- 「自有应用」：在 Microsoft Entra 注册应用，支持的帐户类型选「任何组织目录中的帐户和个人 Microsoft 帐户」，平台选 Web，重定向 URI 填写登录页面显示的回调地址，并在「证书和密码」中创建客户端密码。
- 只覆盖 global 区域的个人账号和工作或学校账号。世纪互联、美国政府、德国区和 SharePoint 使用原生表单；对这类存储发起重新授权会被拒绝。新存储写入 `region=global`、`is_sharepoint=false`。

### Google Drive

- 默认「粘贴令牌」：登录页打开 api.oplist.org，选择「Google Drive OAuth2 (OAuth App)」，阅读弹出的 Google 授权说明后勾选「使用 OpenList 提供的参数」，点「获取 Token」登录 Google 账号，复制刷新令牌粘贴回登录页。这里不预选选项，以免跳过授权页的 Google 说明。
- 「自有应用」：在 Google Cloud 控制台启用 Google Drive API，配置 OAuth 同意屏幕，创建「Web 应用」类型的 OAuth 客户端，并在已获授权的重定向 URI 中登记回调地址。同意屏幕处于「测试」状态时 refresh token 7 天后失效。Google 只在首次授权时返回 refresh token，授权请求因此带 `prompt=consent`。
- 只接入「我的云端硬盘」，共享云端硬盘还没有验证。

## API 上游（goapi，connect 4）

`goapi` 不是网盘：它把**另一个 goapi 服务**（例如另一台机器上的 cloud2api）作为存储接入。在 connect 向导中选择该平台，填写远端服务的地址与 API Key，由新增的 GoAPI 存储驱动保存并访问；适配器只负责把这两项写进驱动字段，cloud2api 不经手。隔离环境的配对验收覆盖了回收站恢复、上传回执与字节一致性、跳数环路有界及免登返回流程。第三方通用 GoAPI/Gokapi 仍不受支持；这些测试不代表真实远端账号或网盘上传已全部验证。下方记录表仅记录网盘账号实测。

- **协议限定**：只支持远端服务的 cloud2api `/v1` 分层协议，不提供通用 REST 兼容——这不是任意 GoAPI/Gokapi 网盘服务的接入方式。Gokapi 兼容层的扁平上传 API（`/api/files/add`、`/api/chunk/*` 等）不作为上游协议，不受支持。
- **列表缓存**：list 结果可能滞后至存储配置的缓存过期时间（默认约 30 秒）；stat/Get 与类型化操作结果是实时新的，需要最新列表时手动刷新即可更新。
- **操作范围**：list/stat 为代理流式读取；mkdir/move/delete 按对端 Key 的实际权限放行，对端拒绝时原样返回错误；copy 借助 OpenList 转存回退完成。整个存储的可见范围就是对端 Key 授予的网盘、根目录和权限。
- **上传与回执**：上传要求已知长度（Content-Length）；零字节与 multipart 的支持范围属计划项，以测试确认为准。成功判定前严格核对上游回执的路径、大小、CRC-64 与 SHA-256；结果未知、不匹配或无响应时只报告「未确认」，不宣称已回滚，也不做盲目重试。
- **跳数保护（显式能力协商）**：对端须在 `/v1/me` 中显式声明**串联跳数上限**能力（字段 `relay_hop_limit`，取值必须等于 4），驱动校验通过才允许接入；缺少该能力或取值不符的旧版对端一律拒绝，必要时先升级对端。混合版本的对端可能在转发时丢弃跳数头，只靠请求头无法封顶环路，因此必须显式协商。校验通过后，请求头 `X-GoAPI-Hop`（整数 0–4）随请求沿 cloud2api → OpenList → GoAPI 驱动 → 对端服务逐跳传播；大于 4 或格式非法的取值被拒绝。不做数据库迁移，不引入持久实例 ID 或拓扑记录；上游侧由补丁 0003 使中继闭环在跳数上限处终止而不是挂起。
- **系统目录（`system_ns`）**：驱动在初始化探测成功后为该存储生成一次随机 128 位系统命名空间并随存储保存（重新授权保留；向导无此输入项，不引入新凭据）。虚拟路径 `/.c2a-trash` 与 `/.relay` 映射到所配置根目录下的存储私有普通目录 `.goapi-system-<ns>-<存储 ID>-trash` / `-relay`。请勿在对端手动修改、复制或清空这些目录，否则虚拟别名悬空。寻址边界：物理别名在虚拟视图中不可直接寻址；虚拟 `/.c2a-trash`、`/.relay` 按设计供网关内部与原生管理台使用；外部经 cloud2api `/v1` 的保留路径守卫不变。
- **兼容性边界**：cloud2api 不承诺该驱动与所有 goapi 实现兼容：只接入支持上述 `/v1` 语义、回执校验并声明跳数保护的 cloud2api `/v1` 上游；旧 `/v1` 实例不自动兼容。不宣称固定的部署拓扑。

## 回调地址与 HTTPS（仅自有应用）

Microsoft 和 Google 只接受 HTTPS 回调地址，`localhost` 例外。OpenList 只通过局域网 HTTP 地址访问时，有两种做法：

1. 通过 HTTPS 反向代理访问 OpenList，并在 OpenList 配置中设置 `site_url`，登录页面会据此给出完整的回调地址。
2. 在应用中登记 `http://localhost:5244/api/connect/oauth/callback`。授权完成后浏览器会跳转到一个打不开的 localhost 地址，这时把地址栏中的完整地址复制，粘贴到登录页面的「回调地址无法打开」输入框。授权码只能使用一次，且必须与当前登录会话的 state 匹配。

默认方式（扫码、粘贴令牌）不需要回调地址。

## 示例适配器

`example-openlist` 用账号密码连接另一个 OpenList（上游 `OpenList` 驱动），只在 `loginsdk_example` 构建标签下注册，不进入生产镜像。它是贡献者的参考实现，也是 `scripts/e2e.sh` 的测试载体，可以在没有任何网盘账号的情况下跑通新建、读写、重新授权和各类失败路径。

## 验证记录

每个平台分别记录新建与读写、重新授权、错误路径、真实续期、失败恢复和无泄露检查，按 [真实网盘账号测试清单](../../docs/REAL-ACCOUNT-TEST.md) 记录过程。未测的项留空。

| 日期 | 平台 | 账号类型 | 登录方式 | 补丁版本 | 1 新建与读写 | 2 重新授权 | 3 错误路径 | 4 真实续期 | 5 失败恢复 | 6 无泄露 | 备注 |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| 2026-10-14 | 123云盘 | — | 账号密码 | connect 1 | 新建通过（状态 work） | | | | | | 用户实测 |
| 2026-10-14 | 阿里云盘 | — | 在线授权服务扫码 | connect 1 | 新建并纳管通过（状态 work） | | | | | | 用户实测 |
| 2026-10-14 | 百度网盘 | 未说明 | 未说明 | connect 2 | 登录通过；读写未逐项记录 | | | | | | 用户反馈「没问题」 |
| 2026-10-14 | OneDrive | 未说明 | 未说明 | connect 2 | 登录通过；读写未逐项记录 | | | | | | 用户反馈「没问题」 |
