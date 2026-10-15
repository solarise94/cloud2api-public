[简体中文](README.md) | [English](README.en.md)

<p align="center">
  <img src="internal/admin/web/assets/logo.png" alt="cloud2api 项目 logo" width="200">
</p>

<h1 align="center">cloud2api</h1>

<p align="center">把网盘变成 API，也能直接用网页上传、下载和分享。</p>

cloud2api 基于 OpenList，把不同网盘的文件操作统一成一套接口。你可以在网页里管理文件，也可以给脚本或 AI Agent 一个 API Key，让它操作指定的网盘和目录。

## 能做什么

- **上传和分享**：拖入文件上传，完成后手动生成有有效期的下载链接，发给别人。
- **管理文件**：浏览目录、下载、新建文件夹、重命名、删除，以及复制和跨盘转存。
- **给脚本和 Agent 用**：同一套 API 操作不同网盘，不用分别适配各家接口。
- **按需授权**：每个 Key 可以限定网盘、目录和权限，提供「通用」「只读」「全部」三个权限组。
- **记住登录**：文件工作区默认保持登录 30 天，可以收藏入口；管理员登录默认保持 24 小时。
- **连接另一台 cloud2api**：在添加网盘时选择 API 上游，填写对端地址和 Key。

网盘登录和连接设置交给 OpenList，cloud2api 负责文件页面、API 和授权管理。登录向导提供百度网盘、阿里云盘、123云盘、OneDrive、Google Drive；其他驱动可以在 OpenList 后台添加。Google Drive 暂未完成真实账号验证，具体情况见[支持列表](openlist-patch/docs/PLATFORMS.md)。

## 怎么部署

准备一台 Linux 机器，安装 Bash、Python 3 和可用的 rootless Podman（含 `slirp4netns`）。构建会在容器里完成，不需要在宿主机安装 Go 或 Node.js。

以下是首次部署的步骤：

```bash
git clone https://github.com/solarise94/cloud2api-public.git
cd cloud2api-public

# 已有配置不要覆盖。
if [ ! -e config.yaml ]; then
  install -m 600 config.production.example.yaml config.yaml
fi

PUBLISH_ADDR=127.0.0.1 \
OPENLIST_PUBLISH_ADDR=127.0.0.1 \
OPENLIST_PUBLIC_URL=http://127.0.0.1:5244 \
SEED_STORAGES=0 \
bash scripts/deploy.sh
```

首次构建会下载依赖并启动 cloud2api 和 OpenList。上面的配置仅供本机访问；从其他设备使用时，需要调整发布地址和 `OPENLIST_PUBLIC_URL`，公网访问请使用 HTTPS。

| 入口 | 地址 |
| --- | --- |
| 首页 | `http://127.0.0.1:8080/` |
| 文件工作台 | `http://127.0.0.1:8080/files/` |
| 管理后台 | `http://127.0.0.1:8080/admin/` |
| OpenList 后台 | `http://127.0.0.1:5244/@manage` |

管理员密码保存在 `cloud2api-data/admin_password`，OpenList 密码保存在 `cloud2api-data/openlist_admin_password`。升级前备份 `config.yaml`、`cloud2api-data/` 和 `openlist-data/`，保留已有配置和数据。

## 网页怎么用

1. 打开管理后台，输入管理员密码。
2. 点击「添加网盘」，按向导登录网盘并保存，再回到 cloud2api 纳管。
3. 创建 API Key，选择允许访问的网盘和目录。一般选「通用」；只想查看和下载就选「只读」。「通用」不包含删除。
4. 打开文件工作台，用这个 Key 登录。只授权一个网盘时直接进入，多个网盘时先选盘。
5. 拖入文件上传；需要分享时，点击「获取链接」并设置有效期，最长 7 天。

上传不会自动公开文件。工作区地址可以收藏，但别人只有地址、没有登录记录，也不能打开你的文件。

## 有哪些 API

脚本和 Agent 使用 API Key 认证，放在 `apikey` 请求头中，也支持 `Authorization: Bearer <API Key>`。`{drive}` 是 `/v1/me` 返回的网盘 ID，文件路径相对于这个 Key 获准访问的根目录。

常用接口如下：

| 方法 | 接口 | 用来做什么 |
| --- | --- | --- |
| GET | `/v1/me` | 查看可用网盘和权限 |
| GET | `/v1/drives/{drive}/list` | 列出目录里的文件 |
| GET | `/v1/drives/{drive}/stat` | 查看文件信息 |
| GET / PUT | `/v1/drives/{drive}/files` | 下载 / 上传文件 |
| POST | `/v1/drives/{drive}/mkdir` | 新建目录 |
| POST | `/v1/drives/{drive}/delete` | 删除文件或目录 |
| POST | `/v1/drives/{drive}/move` | 同盘移动或重命名 |
| POST | `/v1/transfers` | 复制文件或跨盘移动 |
| GET | `/v1/jobs/{id}` | 查询异步任务结果 |
| POST | `/v1/links` | 生成限时分享链接 |
| GET | `/l/{token}` | 通过分享链接下载，不需要 Key |

### 先看看能访问哪些网盘

```bash
export C2A_BASE_URL=http://127.0.0.1:8080
read -r -s -p 'API Key: ' C2A_API_KEY
printf '\n'
export C2A_API_KEY

curl --fail-with-body \
  -H "apikey: $C2A_API_KEY" \
  "$C2A_BASE_URL/v1/me"
```

### 列目录、上传和下载

把 `your-drive-id` 换成上一步返回的网盘 ID：

```bash
DRIVE_ID=your-drive-id

# 列出根目录。
curl --fail-with-body -G \
  -H "apikey: $C2A_API_KEY" \
  --data-urlencode 'path=/' \
  "$C2A_BASE_URL/v1/drives/$DRIVE_ID/list"

# 上传本地文件，保存为网盘里的 /example.txt。
curl --fail-with-body -X PUT \
  -H "apikey: $C2A_API_KEY" \
  -H 'Content-Type: application/octet-stream' \
  --data-binary @./example.txt \
  "$C2A_BASE_URL/v1/drives/$DRIVE_ID/files?path=/example.txt"

# 下载到本地。
curl --fail-with-body \
  -H "apikey: $C2A_API_KEY" \
  -o downloaded.txt \
  "$C2A_BASE_URL/v1/drives/$DRIVE_ID/files?path=/example.txt"
```

上传以接口返回的确认结果为准；如果操作返回异步任务 ID，继续查询 `/v1/jobs/{id}`，不要把「已接受」当作「已完成」。

### 接入 AI Agent

把你自己的服务地址和 API Key 配给 Agent，再提供以下任一接入资料：

- `/custom/agent-instructions.txt`：Agent 使用说明。
- `/custom/SKILL.md`：Skill 文档。
- `/apidocumentation/openapi.json`：常用接口的 OpenAPI。
- `/custom/c2a.py`：Python 命令行工具。

## 更多用法

- [文件工作台](docs/FILE-WORKSPACE.md)：上传、分享、目录管理和记住登录。
- [权限与中转](docs/API-KEYS-AND-RELAY.md)：Key 设置、Gokapi 兼容上传，以及独立的 `/relay/` 上传页。
- [API 开发文档](docs/API-DEVELOPMENT.md)：请求参数、返回格式、高级接口和兼容接口。
- [OpenList 补丁说明](openlist-patch/README.md)：登录向导、构建和适配器开发。
