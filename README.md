<p align="center">
  <img src="internal/admin/web/assets/logo.png" alt="cloud2api 项目 logo" width="200">
</p>

<h1 align="center">cloud2api</h1>

<p align="center">自托管多网盘 API 网关 · API Key 授权 · Agent 文件工具</p>

cloud2api 将 OpenList 管理的存储转换为面向工具和 AI Agent 的统一文件接口，同时提供供人使用的「我的文件」工作台。管理员先在 OpenList 中连接网盘，再通过 cloud2api 管理门户纳管存储、创建受限 API Key；人和 Agent 都只使用被授予的网盘、目录和能力。

**当前为 v0.4 开发阶段。** 接口、权限边界和本地模拟存储有自动化测试，但真实网盘的账号类型、长期续期、限流和故障恢复仍需验证。请先用测试账号及专用目录试用，再用于重要数据；不要把它当作已完成全部网盘认证的商业服务。

- [功能与架构](#功能与架构)
- [网盘登录支持](#网盘登录支持)
- [快速开始](#快速开始)
- [API Key 权限与根目录](#api-key-权限与根目录)
- [Agent API 使用示例](#agent-api-使用示例)
- [API 中转与上传回执](#api-中转与上传回执)
- [配置、数据与升级](#配置数据与升级)
- [安全与公开发布](#安全与公开发布)
- [开发与测试](#开发与测试)
- [文档与贡献](#文档与贡献)
- [许可证](#许可证)

## 功能与架构

### 主要能力

- **我的文件工作台**：以「上传文件 → 获取链接」为主要流程，大面积拖放上传区、独立上传记录与限时公开下载链接；文件浏览与整理默认展开在上传区下方，沿用管理门户的视觉风格和深浅色主题。
- **统一文件 API**：列目录、读取、上传、建目录、移动、复制及跨盘转存；路径限定在 Key 的授权根目录内。
- **面向 Agent 的基础工具**：授权发现、文件读写、目录管理、移动复制、长任务结果和限时分享；默认接入资料只展示这一层。
- **保留的高级能力**：目录树、搜索、批处理、URL 转存、诊断和受限子 Key 等保留实现与权限检查，完整说明放在[开发接口文档](docs/API-DEVELOPMENT.md)，不混入默认 Agent 提示。
- **管理门户**：发现与纳管 OpenList 存储，管理 API Key、权限组、有效期、限额和回收站策略。
- **存储身份保护**：跟踪 OpenList storage ID；原存储被删除、同挂载点出现新存储时，不自动继承授权，需管理员确认替换。
- **登录适配补丁**：在 OpenList 内提供最小登录向导、可选自有应用配置和保存后的返回流程；保留上游原生管理后台。connect 4 支持管理员同源免登交接与 `goapi` 上游适配，详见下文。
- **API 上游存储**：把另一个 goapi 服务的 `/v1` 分层接口作为存储接入（connect 4）；地址与 Key 只保存在 OpenList 的 GoAPI 驱动中。
- **Gokapi 兼容中转**：上传、分块、文件列表、下载分享和可查询上传回执；另有独立、未与门户互链的本地中转上传页 `/relay/`。

### 两个服务，明确分工

```text
管理员浏览器
  ├─ OpenList /@connect/ 或原生后台
  │    └─ 网盘登录、凭据、存储配置、驱动初始化与令牌续期
  ├─ cloud2api /admin/
  │    └─ 纳管映射、API Key、权限、限额与审计
  └─（connect 4）门户「添加网盘」经同源免登交接打开向导，
     不向浏览器写入 OpenList 密码或管理令牌

Agent / 脚本 / 中转浏览器
  └─ cloud2api：校验 Key、网盘范围、根目录和权限
       └─ OpenList：执行真实网盘文件操作（含 GoAPI 驱动访问远端 goapi 上游）
```

**cloud2api 不提供网盘凭据表单，不写 OpenList 的存储配置。** 网盘的添加、重新授权、参数、启停和删除由 OpenList 完成；上游 API 存储（`goapi`）的服务地址与 Key 也只填写并保存在 OpenList 侧的 GoAPI 驱动中，cloud2api 不经手。cloud2api 仍需要自身管理员密码、API Key 和服务端 OpenList 管理令牌；这些不是网盘登录凭据。

最小登录页不实现图库、媒体播放、Office 或预览。项目不重新编写网盘驱动，也不以新的第三个登录服务替代 OpenList。

## 网盘登录支持

默认构建使用 OpenList **v4.2.6** 与本仓库的 **connect 4** 补丁，固定的上游提交和构建参数见 [openlist-patch/UPSTREAM](openlist-patch/UPSTREAM)。下表的云盘登录实测反馈在 connect 3 期间收集；connect 4 的 `goapi` 上游适配与免登交接已通过隔离环境的配对功能验收，不代表所有真实上游都已验证。

| 平台 | 登录向导默认方式 | 真实账号验证状态 |
| --- | --- | --- |
| 百度网盘 | 公开应用设备码／二维码授权 | 已有用户登录成功反馈 |
| 阿里云盘 | 在线授权服务扫码 | 已有用户登录成功反馈 |
| 123云盘 | 账号密码 | 已有用户登录成功反馈 |
| OneDrive | 在线授权服务取得令牌后粘贴 | 已有用户登录成功反馈 |
| Google Drive | 在线授权服务取得令牌后粘贴 | **尚未实测** |
| API 上游（`goapi`） | 填写远端服务地址与 API Key（connect 4） | 配对功能验收通过 |

以上状态截至 **2026-10-14**。登录成功不代表所有应用模式、账号类型、读写、重新授权、长期续期和错误恢复均已通过。详细前置条件、驱动对应关系和测试范围见 [平台矩阵](openlist-patch/docs/PLATFORMS.md)。

自有应用是可选方案，不要求每位使用者先配置客户端密钥。免配置方式可能依赖公开应用或 `api.oplist.org` 等第三方授权服务，其可用性及服务条款不由 cloud2api 保证；请按驱动文档选择符合自身需求的方式。

**其他 OpenList 驱动也可使用**：先在 OpenList 原生后台配置，再回 cloud2api 纳管。不必等待登录向导适配器。新的适配器在构建时注册，贡献流程见 [适配器开发指南](openlist-patch/docs/CONTRIBUTING-adapters.md)。

### API 上游存储（connect 4，已通过配对功能验收）

「添加网盘」除了网盘登录，还支持把**另一个 goapi 服务**（例如自己另一台机器上的 cloud2api）作为存储接入：在 OpenList connect 向导中选择 `goapi` 平台，填写远端服务的地址与 API Key，保存后像普通网盘一样纳管、授权。

- **只支持 `/v1` 分层协议**：列目录/读取为代理流式转发；建目录、移动、删除按对端 Key 的实际权限放行；复制借助 OpenList 转存回退。上游仅面向 cloud2api `/v1`（并要求 `relay_hop_limit=4` 能力声明），不是任意通用的 GoAPI/Gokapi 网盘服务：Gokapi 兼容层的扁平上传 API **不作为上游存储协议**，本项目的 `/relay/` 也只服务本网关自己的中转上传。
- **凭据位置**：上游地址与 Key 只保存在 OpenList 的 GoAPI 驱动中；cloud2api 不保存、不回显。
- **上传与回执**：上传要求已知长度（零字节与 multipart 的支持范围以测试确认为准）；成功前严格核对回执的路径、大小、CRC-64 与 SHA-256，结果未知或不匹配只报告「未确认」，不宣称回滚，也不盲目重试。
- **跳数保护（显式能力协商）**：仅支持启用了**串联跳数保护**的 cloud2api `/v1` 上游——对端须在 `/v1/me` 中显式声明跳数上限（字段 `relay_hop_limit`，取值必须等于 4），否则驱动拒绝接入；必要时先升级对端，**并非所有旧 `/v1` 实例都自动兼容**。链路以 `X-GoAPI-Hop`（0–4）计数，超限或格式错误的请求被拒绝；环回与多跳场景已在配对验收中验证有界。不宣称固定拓扑。
- **列表缓存**：列目录结果可能滞后至存储配置的缓存过期时间（默认约 30 秒）；stat/Get 与类型化操作结果是实时新的，需要最新列表时手动刷新即可更新。
- **对端系统目录**：驱动为存储生成一次私有系统命名空间（向导无此输入项，不引入新凭据），`/.c2a-trash` 与 `/.relay` 映射为对端所配置根目录下 `.goapi-system-<ns>-<存储 ID>-trash` / `-relay` 的普通目录；网关回收站/恢复与中转上传经此照常工作。请勿在对端手动修改、复制或清空这些目录（虚拟别名会悬空）。寻址边界：物理别名在虚拟视图中不可直接寻址；虚拟路径按设计供网关内部与原生管理台使用；外部经 `/v1` 的保留路径守卫不变。
- **免登交接**：管理员在门户发起时，若 OpenList 支持（`module.handoff`），向导经同源一次性票据免登打开；浏览器不会取得 OpenList 管理密码、完整管理 JWT 或长期管理令牌。原生 `/@manage` 后台仍需手动输入 OpenList 密码。旧模块自动回退为手动打开链接。

## 快速开始

下面面向**全新部署**，默认只发布到本机回环地址，不添加模拟网盘。现有部署请先阅读 [配置、数据与升级](#配置数据与升级)，不要覆盖已有 `config.yaml`。

### 1. 环境要求

- Linux、Bash，以及可正常使用的 **rootless Podman**；脚本使用 `slirp4netns`，两个容器共享网络命名空间。
- 能拉取基础镜像、上游源码及 Go 依赖的网络。首次构建需要一定时间、磁盘和内存。
- 本机部署脚本使用常见 Linux 工具；API 示例使用 `curl`，回执示例及验收脚本使用 Python 3。
- **运行不需要宿主机 Go 或 Node.js**。Go 和补丁的构建在容器中进行；Node.js 只用于开发时的前端检查。

现有脚本以 Podman 为准，不包含 Docker Compose、Kubernetes 或完整公网生产部署方案。源码中的 Containerfile 可以单独构建，但其他拓扑需要自行设置端点、卷和网络。

### 2. 获取代码并使用空白生产配置

```bash
git clone https://github.com/solarise94/cloud2api-public.git
cd cloud2api-public

# 只在 config.yaml 尚不存在时复制；保留现有部署的配置。
if [ ! -e config.yaml ]; then
  install -m 600 config.production.example.yaml config.yaml
fi
```

[生产配置示例](config.production.example.yaml) 不包含预设网盘或 API Key：通过门户创建自己的纳管映射和 Key。[config.example.yaml](config.example.yaml) 则用于模拟存储开发测试，不要把它与生产配置混淆。

### 3. 构建并启动

```bash
PUBLISH_ADDR=127.0.0.1 \
OPENLIST_PUBLISH_ADDR=127.0.0.1 \
OPENLIST_PUBLIC_URL=http://127.0.0.1:5244 \
SEED_STORAGES=0 \
bash scripts/deploy.sh
```

脚本构建 cloud2api；所需补丁镜像不存在时，会构建带登录模块的 OpenList。默认本机入口：

| 入口 | 地址 | 用途 |
| --- | --- | --- |
| 我的文件 | `http://127.0.0.1:8080/files/`（根地址自动跳转） | 使用受限 API Key 浏览网盘、上传与分享 |
| cloud2api 管理门户 | `http://127.0.0.1:8080/admin/` | 管理员密码登录、纳管与授权 |
| 中转上传工具 | `http://127.0.0.1:8080/relay/` | 仅限 API Key 的本地中转，未与门户互链 |
| OpenList 登录向导 | `http://127.0.0.1:5244/@connect/` | 网盘登录与存储保存 |
| OpenList 原生后台 | `http://127.0.0.1:5244/@manage` | 高级存储管理 |
| 健康／就绪检查 | `http://127.0.0.1:8080/healthz`、`/readyz` | 服务与上游检查 |

首次部署生成两个独立管理员密码，分别保存在 `cloud2api-data/admin_password` 和 `cloud2api-data/openlist_admin_password`，权限设为 `600`；已有密码文件会复用。**不要将首次终端输出、密码文件或运行目录提交到仓库或贴到 Issue。**

首次部署使用空白生产配置时，不会预设可用的 Agent Key。要使用接口，需先完成下一步。

### 4. 连接、纳管并创建 Key

1. 登录 cloud2api 管理门户，通过「添加网盘」进入 OpenList 登录向导：选择**网盘登录**（现有平台适配器）或 **API 上游**（connect 4——在向导中填写远端 goapi 服务地址与 Key，保存为 GoAPI 存储）。也可直接使用 OpenList 原生后台。
2. 在 **OpenList** 完成授权或填写上游参数并保存存储。回到 cloud2api 后刷新，确认并纳管该存储。
3. 创建 API Key，选择具体网盘、默认位置和根目录；一般使用默认「通用」权限组。
4. 妥善保存仅创建／轮换时返回的 Key 明文。先调用 `/v1/me` 确认授权，再执行文件操作。

OpenList 支持免登交接（connect 4，`module.handoff`）时，门户「添加网盘」经同源一次性票据直接打开向导，无需再输入 OpenList 密码；模块不可用或较旧时自动回退为打开登录链接手动进入。原生 `/@manage` 后台始终需要 OpenList 密码手动登录。向导保存后经门户自带的公开中转页回到网盘页并刷新纳管状态：直接从 OpenList 跳回门户是跨站点导航，不带 `SameSite=Strict` 的门户会话 Cookie，中转页先在门户源上加载静态页面（不下发 Cookie、不是登录入口），再跳回固定地址完成同源二次跳转；取消、保存失败等未取得存储 ID 的场合经不带查询参数的中转页回列表，带 `storage_id` 时只接受唯一正整数，空值、格式不符、重复或多余参数返回 400；Cookie 策略不因此放宽（connect 4）。

使用没有补丁的上游 OpenList 也可纳管存储；登录向导不存在时，使用原生后台。通过 `OPENLIST_IMAGE` 指定上游镜像，或按 [补丁构建说明](openlist-patch/README.md) 构建指定版本。

## API Key 权限与根目录

| 权限组 | 内容 | 适用场景 |
| --- | --- | --- |
| **通用（默认）** | `list read write transfer link fetch relay` | 日常读写、上传下载、转存、直链和中转 |
| **只读** | `list read` | 列目录和读取／下载，不授予写入或分享能力 |
| **全部** | 十项权限，含 `delete purge delegate` | 确需删除、清理和子 Key 的可信工具 |

- 单盘授权一个具体网盘；跨盘必须明确勾选网盘，并在所选范围内指定默认网盘和根目录。新纳管网盘不会自动获得授权。
- `/` 表示**该 OpenList 存储内的 Key 根目录**，不是宿主机路径或跨盘合并目录。可用 `/子目录` 收窄，各盘可以有独立根目录。
- 不指定 drive 的 `/v1/list`、`/v1/files` 等简短调用使用 Key 的默认网盘。
- 「全部」也受网盘白名单、根目录、限额和有效期约束，**不是管理员权限**。
- 通用不含删除，但 `write` 仍允许覆盖、移动等操作；重要数据应使用专用目录和最小权限。
- 高级设置中的全局权限与逐盘权限取**并集**。只取消逐盘项不能撤销仍在全局开启的同一权限。
- 旧自定义 Key 不会因打开编辑页自动转换。静态配置中的旧 `write` 兼容展开规则仍保留，不应把旧配置误认为新的通用权限组。

**不提供 Agent 权限申请／审批模块。** `/v1/requests` 及管理端审批 API 已移除；权限不足由管理员编辑 Key，凭据失效由管理员在 OpenList 中重新授权。Agent 的子 Key 只能在父 Key 明确具有 `delegate` 时创建，且不能扩大父 Key 的范围。

详见 [API Key 权限组与 API 中转](docs/API-KEYS-AND-RELAY.md)。

## 人用文件工作台

在浏览器打开 `/files/`，输入本实例签发的 API Key；仅授权一个网盘时，直接进入该盘的上传与分享页面；多个网盘时先显示「我的网盘」供选择。门户侧栏和 Key 的创建／使用弹窗也有「我的文件」入口。Key 只留在当前页面内存中，刷新或退出后需要重新输入；管理员会话不自动转换为文件访问权限，Key 不会通过链接或新窗口传递。

- 在当前目录拖入或选择多个文件上传，队列分别显示发送进度、等待上游确认和最终结果。断线或取消不代表远端已回滚。
- 下载使用短时单文件凭证交给浏览器原生下载器，不把 API Key 放入链接，也不把大文件整体缓存在网页内存中。进度和结果以浏览器下载列表为准。
- 上传完成后，在上传记录中点击「获取链接」，设置最长 7 天的有效期，再复制公开下载链接发给别人；文件不会随上传自动公开，分享需要 `link` 权限，接收方不需要 Key。链接到期只停止下载，不会删除原文件。
- 文件管理默认展开，提供目录、当前页搜索、新建目录、下载、重命名和删除，也可手动收起。
- 页面按有效授权显示操作：只读 Key 不能上传或分享；通用 Key 不含删除。这里的「我的」指当前 Key 获准使用的范围，不是新增的用户账户体系。
- 这是原网盘目录工作台，不是 `/relay/` 的临时投递箱；密码保护、下载次数限制和全盘搜索不伪装为已支持功能。

工作台随启用的管理门户一起提供，详见[使用与安全边界](docs/FILE-WORKSPACE.md)。开发预览可打开 `/files/?mock=1`，所有演示文件操作只发生在页面内存中，不访问真实网盘。

## Agent API 使用示例

先在门户创建正确授权的 Key。以下命令使用环境变量，避免把明文写进示例文件或 Shell 历史：

```bash
export C2A_BASE_URL=http://127.0.0.1:8080
read -r -s -p 'API Key: ' C2A_API_KEY
printf '\n'
export C2A_API_KEY

curl --fail-with-body -H "apikey: $C2A_API_KEY" "$C2A_BASE_URL/v1/me"
# 从 me.drives 选择已获授权的网盘 ID；路径段需按 URL 规则编码。
DRIVE_ID=your-drive-id
curl --fail-with-body -H "apikey: $C2A_API_KEY" "$C2A_BASE_URL/v1/drives/$DRIVE_ID/list?path=/"
```

数据面接受 `apikey` 请求头或 `Authorization: Bearer <API Key>`，不使用 URL 查询参数或管理员会话进行 Key 认证。

### 上传示例

下面会写入被授权的目录；请先用专用测试文件和测试目录。路径以 Key 根目录为基准，不要填写 OpenList 内部挂载前缀。

```bash
curl --fail-with-body -X PUT \
  -H "apikey: $C2A_API_KEY" \
  -H 'Content-Type: application/octet-stream' \
  --data-binary @./example.txt \
  "$C2A_BASE_URL/v1/drives/$DRIVE_ID/files?path=/example.txt"
```

上传及下载可使用 CRC-64/XZ、SHA-256 校验；支持幂等请求和冲突策略。跨盘操作需分别满足源盘和目标盘的授权，不因指定一个目的盘而获得访问权。

### 可发现的接入资料

| 地址 | 内容 |
| --- | --- |
| `/v1` | 基础接口索引和快速开始 |
| `/v1/me` | 当前 Key 的真实范围、根目录、权限、健康状态与限额 |
| `/apidocumentation/openapi.json` | Agent 基础层 OpenAPI：11 条路径、12 个操作 |
| `/custom/agent-instructions.txt` | 仅包含基础能力的 Agent 接入说明 |
| `/custom/SKILL.md` | 基础能力 Skill |
| `/custom/c2a.py` | 基础命令 CLI，纯 Python 标准库 |

高级接口、Gokapi 兼容层、管理与连接流程保留在[项目开发接口文档](docs/API-DEVELOPMENT.md)及[完整协议快照](docs/openapi.full.json)中，不由默认 Agent 资料引导发现。完整 CLI 保留在 [`tools/c2a-full.py`](tools/c2a-full.py)，供开发者和既有高级脚本使用。

这是文档与发现面的分层，**不是关闭接口或改变授权**。`me` 仍如实报告权限及 GoAPI 串联所需的能力字段；API 上游连接仍由管理员配置。既有路由、默认网盘快捷别名和兼容客户端不因文档收敛而失效；原先依赖在线 CLI 高级命令的用户应改用仓库中的完整 CLI。

这些公开说明不包含 Key 明文。实际文件操作仍需认证；安装说明或 CLI 前，应确认它们来自自己的可信服务地址。

## API 中转与上传回执

`/relay/` 是一个**独立、未与门户互链**的本地「中转上传工具」页面：管理登录页不再提供「API 中转登录」标签，门户内也没有指向它的入口；需要时直接在浏览器打开 `/relay/`。在页面粘贴具有 `relay` 及所需文件权限的 Key。它只保存在页面内存，刷新或退出后需重新输入；此页不签发管理员会话，也不能修改网盘或授权。

中转只面向本网关已纳管的存储。API 上游存储（`goapi`）只通过 `/v1` 分层接口访问；Gokapi 扁平上传 API 不作为上游协议，中转接口也不会把请求转发给远端 Gokapi 服务。

兼容层实现上传、分块提交、自己的文件列表、下载、删除／恢复和分享页面等接口，**不是完整 Gokapi 服务**。用户管理、Key 管理、日志等管理接口不对数据面开放；密码保护上传等未支持项会明确拒绝。删除和恢复还需要显式 `delete` 权限。

默认单文件上限为 8 GiB、中转配额为 200 GiB、最长保留 7 天；Key 配置及兼容接口返回的限额为准。上传后的分享地址可能无需 Key 即可访问，因此中转不等于私人文件保险箱。

```bash
UPLOAD_ID="$(python3 -c 'import uuid; print(uuid.uuid4())')"

curl --fail-with-body \
  -H "apikey: $C2A_API_KEY" \
  -H "X-Upload-ID: $UPLOAD_ID" \
  -F 'file=@./example.txt' \
  -F 'expiryDays=7' \
  -F 'allowedDownloads=0' \
  "$C2A_BASE_URL/api/files/add"

curl --fail-with-body \
  -H "apikey: $C2A_API_KEY" \
  "$C2A_BASE_URL/api/files/status/$UPLOAD_ID"
```

`Status` 是模型判断上传结果的依据：

- `uploading`：仍在接收、提交或等待上游，不能报告完成。
- `completed`：上游已确认成功，文件元数据已保存。
- `failed`：收到明确错误；不代表远端没有残留文件。
- `interrupted`：连接、取消或服务中断；远端可能已有部分或完整文件。

**进度 100%、HTTP 200、文件信息或分片暂存均不能单独代表完成。** 同一 Key 可查询持久化回执；其他 Key 或未知 ID 均不获得该回执。断线、查不到或超时只能报告「结果未确认」。

`X-Upload-ID` 对应一次尝试，直接上传重试必须使用新 ID；旧 ID 的结果不能被覆盖。重试前先查原结果，避免重复提交。分块暂存可复用同一 UUID，最终提交只有一个工作者进入上游。

## 配置、数据与升级

### 配置文件

- [config.production.example.yaml](config.production.example.yaml)：空白生产起点，配合本仓库的共享网络命名空间部署。
- [config.example.yaml](config.example.yaml)：模拟网盘与静态 Key 开发示例。
- `config.yaml`：本机实际配置，**不提交到 Git，不复制进镜像**。加载采用严格字段校验，请勿自行添加尚未实现的选项。

`deploy.sh` 为保留现有 Key 和配置而复用已有 `config.yaml`，不会自动修正其端点。共享网络命名空间内的 OpenList 端点应为 `http://127.0.0.1:5244`；其他网络拓扑需自行调整。

### 持久化数据

| 路径 | 内容 |
| --- | --- |
| `openlist-data/` | OpenList 数据库、存储凭据、应用配置及运行日志 |
| `cloud2api-data/` | cloud2api SQLite、Key 哈希、授权、上传回执、临时分块与密码文件 |
| `config.yaml` | 静态网盘／Key 配置及环境变量引用 |

这些目录包含隐私或授权信息，**不是公共项目素材**。限制文件系统权限，保管好备份，不要上传数据库、原始日志、配置或运行目录。Key 明文丢失后应通过管理员轮换，不应从数据库恢复明文。

### 升级与回退

- 升级前用 SQLite backup API 或停机后的可靠备份，同时保存两侧数据库、配置、必要密码文件及原镜像版本。直接复制仍在写入的 `.db` 文件不等于一致备份。
- 新版可能迁移数据库；回退二进制前应确认数据库版本兼容。不要把“替换镜像”当作完整数据回退。
- 将旧配置的 `accounts`／`api_keys` 改成空列表会在启动时清理相应**静态配置行**；不要为套用新示例而覆盖现有部署。
- cloud2api 中的「移出网盘」仅移除映射，不删除 OpenList 存储。实际删除、重新授权、启停和参数修改在 OpenList 中进行。

部署变量和补丁构建流程见 [部署脚本](scripts/deploy.sh) 与 [补丁说明](openlist-patch/README.md)。重新部署真实账号环境保持 `SEED_STORAGES=0`，不要自动重建模拟存储。

## 安全与公开发布

- 本项目是自托管网关，不提供完整多租户账户、计费、托管运维或公网防护方案。
- 部署脚本的历史默认 `PUBLISH_ADDR=0.0.0.0` 会开放 cloud2api 到其他接口；本 README 显式使用回环发布。`OPENLIST_PUBLISH_ADDR` 默认回环，实际端口取决于部署变量。
- 远程使用需要 HTTPS、访问控制和合适的反向代理。内置 HTTP 服务不能防止链路窃听；管理员 Cookie 未强制 `Secure`，不能据此宣称已具备完整生产加固。
- 不要直接把 OpenList 管理端口或管理令牌暴露给公网、Agent 或浏览器脚本。关闭不需要的访客访问，遵循上游安全建议。
- 管理员的 connect 免登交接（connect 4）不改变这条边界：同源校验 + 一次性 60 秒票据只换取 `Path=/api/admin/connect`、30 分钟绝对过期的 `HttpOnly` Cookie；OpenList 管理密码、完整管理 JWT 和长期管理令牌不进入浏览器存储。向导回跳使用的公开中转页 `GET /admin/connect-return` 同样不是登录入口：静态 HTML、不下发 Cookie、只跳固定的网盘地址；带 `storage_id` 时只接受唯一正整数（空值、格式不符、重复或多余查询参数返回 400），无参数时仅用于回列表；也不放宽会话 Cookie 的 SameSite 策略。
- 管理员密码登录与 API Key 数据面分离；API Key 不是管理员凭证。只给 Agent 所需的网盘、子目录、权限和有效期。
- 直链、中转分享属于可公开访问能力，使用前确认文件内容允许分享；撤销 Key、过期与限额行为应先在测试环境验证。
- `.gitignore` 与 `.containerignore`／`.dockerignore` 分别保护提交和构建上下文；忽略规则不能清除已经进入 Git 历史、旧镜像中间层或外部日志的内容。
- 公开前检查所有待推送分支、提交邮箱、旧配置、构建缓存和发布附件。发现真实密钥时先撤销／轮换，再处理历史；不要只删最新文件。

网盘凭据、授权 Cookie、客户端密钥、完整回调 URL、密码文件、数据库和原始请求日志都不应进入公开 Issue、PR 或截图。敏感问题请勿直接贴到公开讨论区。

## 开发与测试

cloud2api 核心开发使用 Go 1.24 或更高版本；补丁构建采用 [固定工具链](openlist-patch/Containerfile)，不要求两者使用同一版本。前端是原生 JavaScript，无需 npm 构建。

```bash
go vet ./...
go test -count=1 ./...
go test -race -count=1 ./...

node scripts/test-key-form.cjs
node scripts/test-relay-page.cjs
node scripts/test-drives-page.cjs
node scripts/test-connect-return.cjs
node scripts/test-agent-onboarding.cjs
node scripts/test-files-page.cjs
python3 scripts/test-agent-helper.py
bash -n scripts/deploy.sh scripts/acceptance.sh
```

竞态检测需要支持的平台和 C 工具链。补丁测试与构建：

```bash
openlist-patch/scripts/test.sh
openlist-patch/scripts/check-bundle.sh
openlist-patch/scripts/e2e.sh
openlist-patch/scripts/build-image.sh
```

`acceptance.sh` 会上传、删除、修改测试存储并写入验收数据库，**不是可安全指向任意部署的健康检查**。只在独立的 Local 模拟存储实例中运行，并显式设置网关、OpenList、密码文件、数据目录及数据库路径；禁止在真实网盘上重建夹具或进行破坏性验收。

自动化模拟测试通过不代表真实网盘续期、限流、故障恢复或每种应用模式都经过验证。真实账号验证请使用专用账号／目录，按 [实测清单](docs/REAL-ACCOUNT-TEST.md) 记录。

## 文档与贡献

| 文档 | 适用内容 |
| --- | --- |
| [接口分层与开发](docs/API-DEVELOPMENT.md) | 基础/高级/兼容/管理边界、完整 CLI 与维护约束 |
| [完整协议快照](docs/openapi.full.json) | 项目开发用全量 Agent/兼容接口，不是默认在线工具清单 |
| [权限与中转](docs/API-KEYS-AND-RELAY.md) | Key 权限、根目录和上传结果 |
| [OpenList 补丁](openlist-patch/README.md) | 上游固定版本、构建、测试与发行说明 |
| [平台矩阵](openlist-patch/docs/PLATFORMS.md) | 默认登录方法和真实账号验证状态 |
| [补丁 API](openlist-patch/docs/API.md) | 登录模块接口约定 |
| [适配器贡献](openlist-patch/docs/CONTRIBUTING-adapters.md) | 新平台注册与示例适配器 |
| [真实账号测试](docs/REAL-ACCOUNT-TEST.md) | 测试记录、故障恢复及脱敏 PR |
| [文件工作台](docs/FILE-WORKSPACE.md) | 上传、公开分享、原生下载与目录导航 |
| [公开快照说明](docs/PUBLICATION.md) | 无历史发布范围、隐私检查与验证边界 |

欢迎贡献代码、文档和测试报告，尤其是 Google Drive、不同 OneDrive 账号类型、自有应用以及其他 OpenList 驱动的验证。

提交报告时说明平台／驱动、版本、账号类型、应用模式和已验证步骤；将“登录成功”“读写成功”“续期成功”分别记录。新增适配器需提供合成数据测试和失败路径，不要提交真实账号凭据或数据库。仅有本地模拟测试时，请明确标注，勿把平台改成“全面验证”。

## 许可证

**cloud2api 核心代码目前尚未指定独立许可证。** 公开可见不等于授予复用、修改或再分发许可；正式发布前需由维护者选择并补充根目录 `LICENSE`，这里不代为假定 MIT、Apache 或其他许可。

OpenList 派生补丁已声明随上游采用 **AGPL-3.0**，许可证文本见 [openlist-patch/LICENSE](openlist-patch/LICENSE)。分发修改后的 OpenList 源码、镜像或二进制时，应保留上游声明，并满足相应源代码提供和修改说明等要求。cloud2api 与 OpenList 为独立服务，不应把两个组件的许可声明混为一谈。

第三方依赖、上游前端和素材还需遵守各自的许可与署名要求；本节不是替代法律审查的保证。
