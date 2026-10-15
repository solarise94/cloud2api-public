# 开发接口分层与完整协议

本文面向项目维护者、管理员和高级集成开发者，**不是默认交给 Agent 的接入提示**。

## 1. 分层约定

| 层级 | 读者与入口 | 行为 |
| --- | --- | --- |
| Agent 基础层 | 在线 instructions、Skill、OpenAPI、`/v1` 索引和基础 CLI | 只介绍日常文件操作及其结果确认 |
| 高级与兼容层 | 本文、完整协议快照、仓库中的完整 CLI | 保留已实现的能力，不在默认 Agent 提示中推介 |
| 管理与连接层 | 管理门户、OpenList 向导及项目维护文档 | 管理员连接与纳管存储、配置 Key 和权限；不是 Agent 数据面 |

**分层不改变授权，不是安全隔离或路由开关。** 已知高级 URL 的客户端仍可调用，但必须通过原来的 Key、网盘范围、根目录、权限、有效期、限额及存储身份检查。管理员 API 保持管理员会话认证，不接受用普通数据面 Key 代替。

已有的全局权限与逐盘权限仍取并集。默认资料不介绍某项能力，不代表它已从 Key 中撤销，也不代表自动授予了基础集合中的所有操作。例如“通用”权限组不包含 delete，基础文档中出现 delete 并不会让该 Key 获得删除权。

完整的 Agent/兼容协议快照位于 [`openapi.full.json`](openapi.full.json)。它保存在仓库中，不额外发布一个在线 full-spec 发现入口。它覆盖 `routeTable` 所登记的协议，不包含管理员路由、OpenList 原生协议或全部历史网关接口。

## 2. Agent 基础层：11 条路径、12 个操作

| 方法 | 路径 | 用途 |
| --- | --- | --- |
| GET | `/v1/me` | 发现授权网盘、根目录、有效权限与健康状态 |
| GET | `/v1/drives/{drive}/list` | 分页列目录 |
| GET | `/v1/drives/{drive}/stat` | 获取单个对象元信息 |
| GET / PUT | `/v1/drives/{drive}/files` | 流式下载 / 上传 |
| POST | `/v1/drives/{drive}/mkdir` | 创建目录 |
| POST | `/v1/drives/{drive}/delete` | 删除路径，按 Key 的回收站策略处理 |
| POST | `/v1/drives/{drive}/move` | 同盘移动或重命名 |
| POST | `/v1/transfers` | 复制或跨盘移动，必要时返回异步作业 |
| GET | `/v1/jobs/{id}` | 查询或长轮询作业结果 |
| POST | `/v1/links` | 创建限时分享链接 |
| GET | `/l/{token}` | 使用分享链接下载，不携带 API Key |

基础层统一介绍路径中的网盘 ID。`drive` 作为单个路径段进行 URL 编码，文件 `path` 则按查询参数或 JSON 字段编码。文件路径始终在 Key 对该盘的根目录内，不填写 OpenList 内部挂载前缀。

`/v1/me` 不隐藏真实的高级权限或兼容元数据，保留 `relay_hop_limit` 等供 GoAPI 上游驱动使用的能力字段；只移除对高级接口的操作引导，不伪造权限。`next_steps` 仍可提示网盘不可用或等待已有作业，但不再建议自动运行 selftest、恢复回收站或打开连接管理流程。

默认 Agent 文档不同时介绍默认盘和指定盘两套文件 URL，也不建议连接后写入测试文件。认证与只读发现从 `me` 开始，实际写入仅在用户任务需要时发生。

### 结果与重试约束

- 原生上传必须检查响应与大小、CRC-64/XZ、SHA-256 等校验结果；仅发送完请求体或收到 HTTP 200 不能替代完成确认。
- `202` 和作业 ID 只表示任务已接受。调用 `GET /v1/jobs/{id}?wait=N`，检查状态及错误，不能直接报告成功。
- delete、move、transfers 的 `dry_run` 与冲突策略保留；大批量或大体积操作先预演。
- `Idempotency-Key` 不能被解释为任何失败都可盲目重试。不确定结果不代表未写入、未移动或已回滚。
- 永久清理与一般删除不同；无需因基础资料介绍 delete 而建议切换“全部”权限组。

## 3. 保留的高级接口

完整请求字段见协议快照及对应源码。下面记录用途、权限边界与维护注意事项，不将其加入默认提示。

| 能力 | 接口 | 维护注意事项 |
| --- | --- | --- |
| 重复的网盘发现 | `GET /v1/drives` | 返回与 `me.drives` 相同生成函数的网盘视图；保留兼容 |
| 默认盘文件别名 | `/v1/list`、`stat`、`files` GET/PUT、`mkdir`、`delete`、`move` | 由 Key 的默认盘解析；不支持靠 `?drive=` 覆盖 |
| 目录树 | `GET /v1/tree`、`GET /v1/drives/{drive}/tree` | 有深度和条目上限，不能无限遍历 |
| 搜索 | `GET /v1/search`、`GET /v1/drives/{drive}/search` | 依索引或有界遍历工作，不保证任意规模全文搜索 |
| URL 转存 | `POST /v1/fetch` | 独立的 fetch 权限及服务端 URL 安全限制，通常返回作业 |
| 批处理 | `POST /v1/batch` | 最多 100 项，逐项鉴权；跨盘复制可转为异步作业 |
| 作业列表 | `GET /v1/jobs` | 仅当前 Key 的作业 |
| 作业取消 | `POST /v1/jobs/{id}/cancel` | 尽力取消底层任务，不保证已经完成的远端写入回滚 |
| 回收站列表 | `GET /v1/trash` | 仅该 Key 可见的记录，普通文件 API 不直接寻址内部回收目录 |
| 恢复 | `POST /v1/trash/restore` | 需 delete 权限及原网盘授权，恢复冲突仍须处理 |
| 永久清理 | `POST /v1/trash/purge` | 独立 purge 权限；不能与一般删除混淆 |
| 子 Key | `GET/POST /v1/keys`、`DELETE /v1/keys/{id}` | 需全局 delegate；子集授权、受限 TTL，撤销父 Key 级联撤销 |
| 活动记录 | `GET /v1/activity` | 仅当前 Key，返回逻辑路径，不泄露内部挂载路径 |
| 写入自检 | `POST /v1/selftest` | 有副作用，不是 liveness/readiness，也不是日常连接必要步骤 |

links、fetch 的指定网盘使用请求体中的 drive；transfers 的源和目标分别有 drive；batch 可逐项指定。不要把这种请求体约定错误地推广成所有文件接口都支持 drive 查询参数。

### selftest 特别说明

当前自检逐盘检查 write 权限，在授权根内写入小文件、读回校验，再由服务端清理。清理不要求调用 Key 具有普通 delete 接口所需的权限；客户端另行调用 delete 时仍需自己的 delete 授权。

当前响应的失败在逐盘结果中报告，通常仍为 HTTP 200。被跳过的无 write 网盘不一定将 `all_ok` 置为 false，故全盘跳过也不能被当作读写验证成功。未来修改应明确 passed/failed/skipped 与测试数量，不能为了让自检变绿自动扩权。

## 4. Gokapi 兼容中继：独立保留

这是一组按文件 ID 寻址的临时投递、分享和回执接口，兼容既有 Gokapi 风格客户端，并供本项目的独立 `/relay/` 页面使用。它不是 OpenList GoAPI 上游挂载协议。

| 范围 | 接口 |
| --- | --- |
| 服务信息 | `GET /api/info/version`、`GET /api/info/config` |
| 上传 | `POST /api/files/add` |
| 分块 | `POST /api/chunk/add`、`POST /api/chunk/complete` |
| 上传回执 | `GET /api/files/status/{uploadId}` |
| 文件元信息 | `GET /api/files/list`、`GET /api/files/list/{id}` |
| 删除与取消延时删除 | `DELETE /api/files/delete`、`POST /api/files/restore` |
| 已认证下载 | `GET /api/files/download/{id}` |
| 分享下载与页面 | `GET /downloadFile?id=…`、`GET /d?id=…`、`GET /h/{hotlinkId}` |
| 旧版兼容接入资料 | `GET /custom/gokapi-agent-instructions.txt` |

兼容接入资料保留给明确使用旧协议的客户端，但不从基础 instructions、Skill、索引或 `me` 中链接过去。它的存在不意味着默认 Agent 应自动加载它。

临时文件隔离于授权根下的 `.relay`，有配额、下载次数和到期策略，最长保留 7 天。回执有 `uploading/completed/failed/interrupted` 状态，只允许原 Key 查询。暂存成功、发送完毕或非阻塞提交不是完成；必须等上游确认并持久化元数据后才能报告 completed。

删除、恢复和分享下载的认证方式并不相同，不能为了减少 URL 合并这些边界。若未来删除兼容层，须先迁移 `/relay/`、存量客户端和上传回执消费方。详细策略见 [API Key 与中转](API-KEYS-AND-RELAY.md)。

## 5. 管理与连接接口

管理门户与 OpenList 登录向导各有认证边界；这些路由不进入 Agent 的基础 OpenAPI。

| 管理能力 | 入口与源码 |
| --- | --- |
| 门户管理员登录与会话 | `/admin/`、`/admin/api/login`、`/admin/api/logout`；`internal/admin/session.go`、`portal.go` |
| 网盘发现、纳管、同步、解绑与身份确认 | `/admin/api/drives` 及其单盘子资源；`internal/admin/drives.go`、`internal/reconcile/` |
| Key 创建、编辑、撤销、轮换 | `/admin/api/keys` 及其子资源；`internal/admin/keys.go` |
| 设置、审计与维护 | 门户相应的 `/admin/api/*`；`internal/admin/portal.go` |
| 最小登录向导与 GoAPI 上游连接 | OpenList `/@connect/`；`openlist-patch/src/server/connect/`、`pkg/loginsdk/` |
| 管理员免登交接 | cloud2api `POST /admin/api/connect/handoff`；OpenList connect-only 票据与 HttpOnly 会话 |
| 向导返回门户 | `GET /admin/connect-return`，公开的固定导航页面，不签发管理员会话 |

流程是管理员在门户发起连接，进入 OpenList 完成登录或配置上游，然后回门户纳管和授权。cloud2api 不接收网盘/上游登录凭据，不写 OpenList 存储配置；上游 URL 和 Key 只在 OpenList 中保存。其自身管理员密码、数据面 API Key 和服务端 OpenList 管理凭据仍按各自职责处理。

GoAPI 驱动由 OpenList 使用远端 `/v1` 层级协议，其文件操作与基础层有重合，不能因为隐藏连接管理说明而移除这些接口。`relay_hop_limit=4` 能力声明与 `X-GoAPI-Hop` 防循环保护必须保留。

详见 [OpenList 补丁说明](../openlist-patch/README.md)、[补丁 API](../openlist-patch/docs/API.md) 与 [平台适配开发](../openlist-patch/docs/CONTRIBUTING-adapters.md)。未提供 Agent 权限申请/管理员审批接口，不应重新引入 `/v1/requests`。

### 人用文件工作台

`/files/` 是人用文件客户端，不是新的管理接口。单一授权网盘在登录后直接进入文件列表，多个网盘先显示选择页；管理员会话不会自动变成数据面 Key，也不为它新增全盘代理。公开静态页与其白名单资源由 `internal/admin/files.go` 提供，文件操作仍使用既有 Key 鉴权及存储身份守卫。默认 Agent 提示无需加载 UI 或增加新操作。

浏览器原生下载使用独立的 UI 辅助协议，**不加入 Agent 基础清单或 `routeTable` 的协议快照计数**：

| 方法 | 路径 | 边界 |
| --- | --- | --- |
| POST | `/v1/drives/{drive}/download-ticket` | 请求体 `{path}`，使用 API Key，需该盘 read 权限；返回 `{url,expires_at}` |
| GET / HEAD | `/download/{token}` | 凭短时单文件凭证流式下载，支持 Range；每次请求重查原 Key 和当前授权 |

凭证使用独立用途标记及 HMAC 签名，固定约 120 秒有效，绑定 Key、网盘、文件、授权根和存储身份；不含 API Key 明文。它与 `/l/{token}` 的公开分享不同，申请不需要 link 权限，也不创建持久分享。允许有效期内重试和分段请求；只在每次请求开始检查到期，已开始的流不因凭证到期主动截断。Key 撤销、过期、read 授权移除、根目录或存储身份变更会使后续兑换失败。凭证依赖持久签名密钥，不依赖进程内存；重启不主动撤销它，轮换签名密钥则使旧凭证失效。返回 no-store/no-referrer，日志与审计不记录凭证 URL；反向代理也应避免记录或长期保留该路径中的 token。浏览器负责实际下载进度与结果，页面不能把提交原生下载当作已完成。

使用、上传确认、公开分享与浏览器凭据边界见 [我的文件工作台](FILE-WORKSPACE.md)。`/relay/` 保持独立临时中继用途，不把其中的下载次数和到期删除策略套用到原生网盘链接，也不把参考 UI 的密码选项宣称为已支持。

## 6. 完整 CLI 与兼容性

- 默认在线 `/custom/c2a.py` 只包含基础命令及基础接口引用。未指定网盘时先从 `me` 解析默认盘，再使用指定盘 URL；link 也支持 `--drive`。基础 CLI 的 cp 只有等到作业 succeeded 才退出 0，失败、取消或等待后仍未完成会非零退出并保留查询提示，不把“已接受”当成完成。
- 完整 CLI 位于仓库 [`tools/c2a-full.py`](../tools/c2a-full.py)，保留原有高级命令供开发者和已有自动化使用；同样使用 `C2A_URL`、`C2A_KEY` 环境变量。
- 原先从在线 helper 下载后调用 tree/search/fetch/selftest/keys/activity/trash 或 jobs list/cancel 的脚本，需要改为使用仓库中的完整 CLI。底层 HTTP 接口未删除。
- 默认盘 HTTP 别名仍保留，但新 Agent 示例优先指定盘路径，减少两套写法的选择负担。
- 旧 `/api/storage/*` 与 `/api/transfer` 不在 Agent 基础资料中；是否在下一主版本移除属于单独的兼容性决策。

## 7. 开发与回归约束

新增接口时必须明确它属于哪一层。完整路由表不自动等于基础白名单，新增高级路径不能因为一个通用 `/v1/` 前缀判断就进入 Agent 工具清单。

- 基础 OpenAPI 与 `/v1` 索引使用相同的显式方法/路径选择规则。
- 基础入口、instructions、Skill、CLI 帮助及 `next_steps` 不引导调用高级、兼容或管理端点。
- 完整协议快照包含已支持的指定盘 tree/search；过去“53 条路径、56 个操作”的数字漏掉了这两项，补齐后登记为 55 条路径、58 个操作。管理端点和历史网关仍不在这个计数中。
- 回归必须同时证明高级路由仍可在原授权下使用，未授权调用仍被拒绝；不能只测试“文档中没有了”。
- `me` 的有效权限、根目录、限额与 GoAPI 能力不能为了视觉简洁而被篡改。
- 本轮不改变 selftest、上传、异步任务、回收站和存储连接的实际协议。

完整快照由全量生成器维护，更新方式：

```bash
C2A_UPDATE_API_DOCS=1 go test ./internal/agentapi -run '^TestFullOpenAPIDocumentation$' -count=1
go test ./...
```

正常回归应检查快照与生成器一致。示例 server 使用保留的文档域名，不写入真实部署地址、账号、Key、存储信息或运行日志。

## 8. 后续资源发现的设计参考

可以借鉴 CLI 到 API 网关中的“供应商注册、列出可选资源、以统一 ID 选择目标”的方式，把不同网盘作为统一资源，而不是要求 Agent 理解每个网盘的登录协议。当前 `me.drives` 已提供授权后的网盘 ID、provider、根目录、权限和健康状态，可作为这种交互的基础。

这是设计参考，**不是把网盘切换成 OpenAI 模型协议的决定**。本轮没有新增 `/v1/models` 或 `/v1/chat/completions`，也不将文件操作伪装成推理请求。未来即便增加其他发现外观，资源 ID 也只能选择目标，不能代替 Key 授权；跨盘操作仍需分别确认源盘与目标盘的权限。现有层级文件 API、管理员连接流程和 OpenList 凭据边界保持不变。
