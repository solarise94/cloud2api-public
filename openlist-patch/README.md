[简体中文](README.md) | [English](README.en.md)

# openlist-patch：带登录适配补丁的 OpenList

cloud2api 的部署由两个服务组成：本目录构建的 OpenList，以及 cloud2api。这里的补丁在固定版本的 OpenList 中加入 connect 模块，用于网盘登录、授权和存储配置；connect 4 起还包含 `goapi` 上游适配（配合新增的 GoAPI 存储驱动）与管理员免登交接（handoff）接口。文件引擎、驱动和续期仍由上游 OpenList 负责。cloud2api only links to the patch pages; it never receives credentials or writes storage config. 当前接口契约见 [API 文档](docs/API.md)，适配方法见 [贡献指南](docs/CONTRIBUTING-adapters.md)。

- 上游版本：OpenList v4.2.6（提交 `2bdf16d`），前端产物 v4.2.6，固定在 [UPSTREAM](UPSTREAM) 中，并校验 sha256。
- 许可证：OpenList 采用 AGPL-3.0，本目录的补丁和新增代码随 OpenList 一同以 AGPL-3.0 发布。

## 目录

| 路径 | 内容 |
| --- | --- |
| `UPSTREAM` | 上游仓库、版本、提交、前端产物地址与校验值、构建镜像、connect 版本号 |
| `patches/0001-register-connect-module.patch` | 唯一的上游接入点：在 `server/router.go` 中调用 `connect.Register(g)` |
| `patches/0002-aliyundrive-open-redact-token-log.patch` | 上游 AliyundriveOpen 驱动在 INFO 日志中打印新旧 refresh token，此补丁去掉日志中的 token |
| `patches/0003-hop-aware-relay-singleflight.patch` | 中继闭环防死锁：`internal/op` 的 list/link/mkdir singleflight 键带上 `X-GoAPI-Hop` 跳数（`src/internal/op/relayflight.go`），中继子请求不再 join 自身阻塞中的祖先 flight，闭环在跳数上限处按普通错误终止（见 N6h）；缓存键保持不带跳数 |
| `src/pkg/loginsdk/` | 登录 SDK：契约、会话管理、注册表、HTTP 工具 |
| `src/pkg/loginsdk/adapters/` | 平台适配器（百度、阿里、123pan、OneDrive、Google Drive，connect 4 起含 API 上游 `goapi`，以及只在示例构建中注册的 example） |
| `src/drivers/goapi/` | connect 4 新增的 GoAPI 存储驱动：把远端 cloud2api `/v1` 实例挂为存储，接入前校验对端 `relay_hop_limit=4` |
| `src/pkg/goapiclient/` | GoAPI 中继协议客户端：能力校验、流式下载、按流式字节核验上传回执 |
| `src/server/goapihop/` | `X-GoAPI-Hop` 跳数头的入站校验与上下文传播 |
| `src/internal/loginbridge/` | 配置适配层：通过 OpenList 自己的存储操作创建存储和重新授权，处理失败清理与恢复，保存应用配置 |
| `src/server/connect/` | HTTP 接口（connect 4 起含免登交接 handoff 与 GoAPI 驱动注册）、OAuth 回调、最小登录配置页面 `/@connect/`（`web/` 下仅有三个文件） |
| `docs/API.md` | connect 模块的接口约定 |
| `docs/PLATFORMS.md` | 首批平台的认证方式、驱动、应用前置条件和真实账号验证状态 |
| `docs/CONTRIBUTING-adapters.md` | 社区增加平台适配器的方法与提交要求 |
| `dev/` | 页面的本地 mock 开发服务器（不进入镜像） |
| `scripts/` | 准备源码树、测试、构建镜像、检查页面产物、端到端测试 |

`src/` 下的文件只会作为新文件复制进上游源码树，`prepare-tree.sh` 会拒绝覆盖任何上游文件，上游代码的改动只有 `patches/` 中的三处。

## 常用命令

```bash
# 单元测试：gofmt、go vet、整程序构建、带与不带示例适配器的测试、-race
OPENLIST_MIRROR=$HOME/src/openlist-upstream openlist-patch/scripts/test.sh

# 构建镜像 localhost/openlist-connect:v4.2.6-connect.<CONNECT_VERSION>（及 :latest）
openlist-patch/scripts/build-image.sh

# 检查登录页面产物（只允许三个文件，不含预览、画廊、影音和 Office 组件）
openlist-patch/scripts/check-bundle.sh

# 端到端测试：构建示例镜像，启动一次性 OpenList，跑完整的新建、读写、重新授权和失败路径
openlist-patch/scripts/e2e.sh
```

`OPENLIST_MIRROR` 指向上游仓库的本地克隆时不访问网络，未设置时从 GitHub 克隆。前端产物缓存在 `~/.cache/openlist-connect/`。构建和测试都在 podman 容器中运行，本机不需要 Go 工具链。

`scripts/deploy.sh` 在没有指定 `OPENLIST_IMAGE` 时使用本目录构建的镜像，镜像不存在时自动构建，`OPENLIST_REBUILD=1` 强制重建。

## 版本追踪

镜像标签为 `<上游版本>-connect.<CONNECT_VERSION>`。`openlist version` 报告上游版本与提交，镜像标签（label）`openlist.upstream.*` 与 `openlist.connect.*` 记录上游版本、connect 版本和补丁源码的 git 版本。运行中的实例在 `GET /api/admin/connect/platforms` 的 `module` 字段中返回同样的信息，cloud2api 门户据此显示登录模块状态。

`conf.Version` 保持上游的 `v4.2.6`：OpenList 按这个版本号执行数据库升级补丁，改动它会让升级逻辑误判。

## 升级上游

1. 修改 `UPSTREAM` 中的版本、提交和前端产物校验值。
2. 运行 `test.sh`：补丁无法应用、上游存储操作语义变化（创建、更新、删除的写库时机）都会在这里失败。`internal/loginbridge` 的测试覆盖了上述语义。
3. 运行 `e2e.sh`，再按 [PLATFORMS.md](docs/PLATFORMS.md) 复核已验证平台。

## connect 4

connect 4 新增能力与修复，行为契约见 [docs/API.md](docs/API.md)：

- **API 上游适配**：平台 ID `goapi` 的适配器配合新增的 GoAPI 存储驱动，把另一个 goapi 服务的 cloud2api `/v1` 分层接口接入为普通存储；上游地址与 Key 只写入该驱动。仅面向 cloud2api `/v1` 上游，不提供通用 GoAPI/Gokapi 兼容：Gokapi 扁平上传 API 不作为上游协议；接入前经 `/v1/me` 显式校验串联跳数保护能力（字段 `relay_hop_limit` 取值必须为 4，驱动校验不通过即拒绝），旧 `/v1` 实例不自动兼容，必要时先升级对端。
- **免登交接（handoff）**：`POST /api/admin/connect/handoff/start` 签发 60 秒单次使用的 32 字节 base64url 票据，公开接口 `POST /api/connect/handoff/exchange` 换取 30 分钟绝对过期、`HttpOnly`、`SameSite=Strict`、`Path=/api/admin/connect` 的 Cookie；向导地址以 URL fragment 携带票据并立即清除。向导页面对 `target` 严格校验（`/new/<platform>`、`/app/<platform>` 的平台必须存在，`/reauth/<storage_id>` 为正 int64）；早期 fragment 双重编码目标的缺陷已修复。`GET /api/admin/connect/platforms` 的 `module.handoff` 布尔位供 cloud2api 协商能力，旧模块回退为手动登录。
- **系统命名空间与虚拟目录**：GoAPI 驱动在初始化探测成功后为存储生成一次随机 128 位 `system_ns` 并随存储保存（重新授权保留；向导无此输入项，不做主数据库迁移，也不引入新凭据）。虚拟路径 `/.c2a-trash` 与 `/.relay` 映射到配置根下存储私有的普通目录 `.goapi-system-<ns>-<存储 ID>-trash` / `-relay`，网关的回收站/恢复与中转上传经此照常工作；请勿在网盘侧手动修改、复制或清空这些目录（会使虚拟别名悬空）。寻址边界：物理别名在虚拟视图中不可直接寻址；虚拟 `/.c2a-trash`、`/.relay` 按设计供网关内部与原生管理台使用；外部经 cloud2api `/v1` 的保留路径守卫不变。
- **缺陷修复**：全量下载对 OpenList `Range.Length=-1` 的处理与上传 Content-Length 的处理两处缺陷已修复并有测试覆盖。驱动另新增**原生 Getter**：`op.Get` 对无 Getter 驱动会在根缓存陈旧时返回 NotFound，驱动自带 Getter 后以此修复根目录建目录映射缺陷，并以 RED/GREEN 集成测试覆盖真实 MakeDir/Get/List 缓存预热/Put/Move/恢复（含 `/data` 根）。补丁镜像已重建。

connect 4 已在隔离环境验证组件测试、构建、竞态检测及多跳配对行为，包括跳数上限、回收站恢复、上传回执与字节一致性。免登交接和返回门户另有浏览器检查；不据此声称真实网盘 GUI 上传、第三方通用 GoAPI/Gokapi、所有账号类型或长期续期都已验证。Google Drive 真实账号仍待测。可复现入口见上文测试命令，社区测试范围见 [真实账号测试指引](../docs/REAL-ACCOUNT-TEST.md)。下节记录仅为 connect 3 的历史验证范围。

## connect 3 验证记录（2026-10-14，最新完成的验证）

本轮只更新百度和 OneDrive 的用户实测标记；阿里、百度、123 和 OneDrive 已收到真实账号登录成功反馈，Google 保留待测标记。登录成功不等于长期续期或所有错误路径均已验证。

- `test.sh`：gofmt、vet、整程序构建、生产/示例注册测试、race 均通过。
- `e2e.sh`：新建、文件读写、失败清理、重新授权恢复、默认登录模式和无凭据泄露等 67 项通过，使用一次性 `v4.2.6-connect.3-example` 实例，没有操作真实存储。
- 欢迎补充未测账号类型和平台，按 [真实账号测试与 PR 指引](../docs/REAL-ACCOUNT-TEST.md#社区测试与-pr) 提交脱敏报告或适配器 PR。
