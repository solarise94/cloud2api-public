[简体中文](CONTRIBUTING-adapters.md) | [English](CONTRIBUTING-adapters.en.md)

# 增加平台适配器

登录 SDK 的适配器随代码构建，在编译时注册，没有动态插件加载。增加一个平台通常只需要提交三样东西：`src/pkg/loginsdk/adapters/<名称>/` 下的适配器包和测试、[`adapters/all/all.go`](../src/pkg/loginsdk/adapters/all/all.go) 中的一行 `Register`，以及 [PLATFORMS.md](PLATFORMS.md) 中的说明。不需要修改其他平台、OpenList 驱动、配置适配层（`internal/loginbridge`）、HTTP 层（`server/connect`）或 cloud2api。

会话、state 与 PKCE、扫码轮询节奏、凭据暂存、保存与失败恢复、页面步骤都由 SDK 统一处理。适配器只描述平台，并把登录结果转换成驱动字段。

## 1. 选择登录方式

从 [example](../src/pkg/loginsdk/adapters/example/example.go) 复制一个包开始，按平台实现下面三种接口之一（定义见 [sdk.go](../src/pkg/loginsdk/sdk.go)）：

| `Meta.Method` | 接口 | 适配器提供 | 参考 |
| --- | --- | --- | --- |
| `oauth` | `OAuthAdapter` | `AuthorizeURL`（SDK 传入 state 与 PKCE）、`Exchange`（兑换授权码） | `baidu`、`onedrive`、`googledrive` |
| `qrcode` | `QRAdapter` | `StartQR`（二维码图片地址或内容）、`PollQR`（映射到六种扫码状态） | `aliyunopen` |
| `form` | `FormAdapter` | `Submit`（校验表单并生成认证配置） | `pan123`、`example` |

每个适配器都实现 `Meta()` 和 `CheckApp(app)`。需要拒绝某些存储的重新授权时（例如 OneDrive 的其他区域），再实现 `ReauthChecker`。

不同应用模式可以使用不同的登录方式：`Meta.Method` 是 `AppModes` 第一项（默认模式）的方式，`Meta.ModeMethods` 为其他模式指定各自的方式，适配器需要实现所有用到的接口。例如百度默认扫码，粘贴令牌用 `form`，自有应用用 `oauth`；OneDrive 和 Google Drive 默认 `form`（粘贴令牌），自有应用用 `oauth`。另有两个可选接口：

| 接口 | 用途 | 参考 |
| --- | --- | --- |
| `PublicAppProvider` | 列出 `public_app` 模式时必须实现，返回所用公开应用的 client_id 和 client_secret | `baidu` |
| `FormGuider` | 表单需要用户先去另一个页面取令牌时，返回打开该页面的按钮和步骤说明 | `baidu`、`onedrive`、`googledrive` |

## 2. 填写 Meta

- `ID`：小写字母、数字、`-` 或 `_`，最长 32 个字符，发布后不再改变。页面地址和应用配置都以它为键。
- `Driver`：必须与上游驱动注册的名称完全一致（`drivers/<名称>/meta.go` 中的 `config.Name`）。重新授权按它匹配现有存储。
- `AppModes`：`own_app`（自有应用）、`online_service`（OpenList 在线授权服务，默认 `https://api.oplist.org`）、`public_app`（上游工具公开发布的应用，见 `PublicAppLabel`），或单独一个 `none`（不需要应用，`none` 不能与其他模式并列）。第一项是默认模式：它不需要管理员填写参数时（`online_service`、`public_app`、`none`），没有保存应用配置也可以直接登录。其他模式由管理员在应用配置中明确选择，不能根据字段是否为空推断。
- `ModeMethods`、`FormLabel`：见上一节；`FormLabel` 是表单方式在页面上的名称，例如「粘贴令牌」，留空时显示「账号密码」。
- `Scopes`、`PKCE`、`AllowOOB`：页面会显示请求的权限范围。提供方支持 PKCE 时必须开启。`AllowOOB` 表示允许把回调地址设为 `oob`、由用户粘贴授权码。
- `FormFields`：表单方式的字段，`Secret: true` 的值会从所有消息中清除。`CreateOnly: true` 的字段只在新建时显示，适配器在重新授权时不能要求它们。
- `RootField` 和 `RootDefault`：驱动的根目录字段，取值为 `root_folder_path` 或 `root_folder_id`。默认值应与驱动的 `DefaultRoot` 一致。
- `ReauthFields`：重新授权允许替换的 addition 键。存储的其他设置都会保留。这里只列凭据和与凭据绑定的续期参数。
- `Notes`：页面上显示的限制和前置条件（中文）。`DocsURL` 指向上游驱动文档。
- `Verified`：真实账号验证记录，例如 `2026-10-20 个人账号`。留空时页面显示「未经真实账号验证」。

`loginsdk.Validate` 会在注册时检查这些规则，不合规的适配器在启动时就会失败。

## 3. 生成认证配置

`Exchange`、扫码确认或 `Submit` 返回 `*loginsdk.AuthConfig`。保存前 SDK 用 `ValidateAuth` 校验它：

- `Driver` 等于 `Meta.Driver`，`AppMode` 等于本次会话的应用模式。
- `Fields`：新建和重新授权都会写入的字段。所有键必须出现在 `ReauthFields` 中。键名是驱动 Addition 的 JSON 键；没有 json tag 的字段使用 Go 字段名，例如 `AccessToken`。
- `CreateFields`：只在新建时写入的连接设置，不能包含根目录字段或任何 `ReauthFields` 中的键。
- `Secrets`：本次登录产生或使用的每个秘密值（token、密码、授权码）。SDK 和配置适配层用它清除响应与日志中的凭据。
- `CredentialType`、`AppIdentity`、`Renewal`、`Account`：页面上显示的脱敏摘要。`Renewal` 用一句话说明驱动之后如何续期。`Account` 是可选的账号名称，查询失败时留空，不能因此让登录失败。

认证字段必须与驱动实际的续期方式一致。例如使用自有应用时写入 `use_online_api=false` 和同一组应用参数，不能沿用驱动默认的在线服务模式。

## 4. 实现约束

- **不刷新令牌。** 续期属于驱动。适配器只兑换授权码，或者只暂存账号密码，由驱动在保存时初始化验证。主动刷新可能轮换 token，造成驱动与适配器同时刷新同一凭据。
- **错误使用 `loginsdk.Errorf` 和错误代码**（`provider`、`network`、`invalid_input`、`app_invalid` 等），消息用中文。提供方返回的错误文本先经 `loginsdk.Scrub` 清除秘密值再放进消息。
- **网络请求使用 `loginsdk.Do` 或 `DoJSON`。** 它们带超时和响应大小限制，连接失败时只报告主机名，不带查询串。
- **提供方地址定义为包级变量**，测试可以把它指向 `httptest` 服务器。
- **不写日志**，也不在错误中包含 token、密码、授权码或应用密钥。
- **扫码**：返回提供方的 https 图片地址（`ImageURL`），或需要页面绘制的文本（`Content`）。提供方的会话标识放在 `Private` 中，它不会发给浏览器。同时填写 `ExpiresAt` 和 `PollEvery`，SDK 每个会话至多每 2 秒轮询一次；提供方要求更长的间隔时（例如设备码授权的 `interval`）按提供方的值填写。`Message` 是等待扫码时显示的提示，例如不扫码时的替代输入方式；`PollQR` 返回的 `Message` 会临时替换它，返回空消息（以及网络错误恢复后）时页面重新显示它，二维码过期后不再显示。
- **粘贴令牌**：用 `loginsdk.ParsePastedToken` 解析用户粘贴的内容，它接受令牌本身、带 `refresh_token` 的 JSON，以及 api.oplist.org 回调地址格式。令牌在保存时由驱动初始化验证，适配器不要为了校验而刷新它。

## 5. 测试

适配器测试与 [baidu](../src/pkg/loginsdk/adapters/baidu/adapter_test.go)、[aliyunopen](../src/pkg/loginsdk/adapters/aliyunopen/adapter_test.go) 相同，用 `httptest` 模拟提供方，至少覆盖：

- `loginsdk.Validate(New())` 通过。
- `CheckApp`：每种应用模式的有效与无效配置。
- 授权地址或二维码请求的参数，包括 state、PKCE challenge 和权限范围。
- 成功兑换后的 `Fields`、`CreateFields`、`Secrets` 和 `AppMode`。
- 提供方拒绝、HTTP 200 中的错误体、缺少 token、网络错误；错误消息中不出现任何秘密值。
- 扫码方式还要覆盖每一种提供方状态到六种扫码状态的映射。

然后运行完整测试（包括 `-race`），并确认 `adapters/all` 的测试仍然通过：

```bash
OPENLIST_MIRROR=$HOME/src/openlist-upstream openlist-patch/scripts/test.sh
```

表单方式的适配器可以参照 `scripts/e2e.sh`，用真实驱动跑一遍保存和失败恢复。

## 6. 真实账号验证与提交

没有真实账号验证的适配器也可以合并，但 `Verified` 必须留空，页面会标注「未经真实账号验证」。有账号的贡献者按 [PLATFORMS.md](PLATFORMS.md#验证记录) 的六项平台验收测试后，在记录表中填写：

- 测试日期，账号类型（个人、企业、会员等级等）。
- 驱动名称，认证方式，应用模式。
- OpenList 版本与 connect 版本（`GET /api/admin/connect/platforms` 的 `module`）。
- 每一项的结果，包括至少一次真实续期，以及遇到的限制。

提交清单：

- [ ] 新增的适配器包与测试，`adapters/all/all.go` 中一行注册。
- [ ] 没有修改其他平台、驱动、`internal/loginbridge`、`server/connect` 或 cloud2api。
- [ ] `test.sh` 全部通过。
- [ ] PLATFORMS.md 中增加平台说明，并填写验证记录或注明未验证。

115 不在首批维护范围内，欢迎有账号的贡献者按本文增加适配。请注明使用的驱动，`115 Cloud` 与 `115 Open` 是不同的驱动，凭据不能混用。
