# workbuddy2api

将已授权的 WorkBuddy / CodeBuddy 账号接入 OpenAI 兼容接口的自托管网关，支持账号池、流式响应、工具调用和 API key 管理。

本仓库基于 [Sliverkiss/workbuddy2api](https://github.com/Sliverkiss/workbuddy2api) 二次开发，保留上游 MIT 许可证。网关与 Web 管理台（`panel/`）在同一个仓库、同一个 Compose 项目里发布，部署一次即可。

## 功能

- 提供 `/v1/chat/completions`、`/v1/responses` 和 `/v1/models`。
- 支持流式与非流式响应、函数工具、自定义工具桥接，以及 JSON Schema 输出校验。
- 保留业务文本、代码、编号和工具参数；正确区分完成、截断和上游错误。
- 支持多账号调度、会话粘性、限流冷却、并发额度与凭据刷新。
- 支持持久化多 API key，创建、启停和删除即时生效。
- API key 可绑定模型白名单，越界模型在选号前被拒。
- 请求日志逐行标明调用密钥（`key=` 列），管理台有独立日志页与按密钥的 token 用量统计页。
- 支持容器内热更新：管理台一键切到新版本，正在进行的对话跑完为止，不需要重新部署。
- 内置网页管理台：账号、API key、排程任务、容器日志，随网关一起启动。
- 提供账号状态、任务查询、任务日志和定时任务控制接口。

## 环境要求

推荐在 Linux 上使用 Docker Engine 与 Docker Compose v2。直接拉取已发布镜像不需要 Go 工具链；从源码构建需要 Go 1.23 或更新版本。登录脚本另需 Bash、Python 3。

需要至少一个本人有权使用的上游账号。模型、额度和客户端是否可用由上游决定，本项目不保证所有客户端都能直接接入。

## 快速开始

以下命令采用管理台默认的 `/opt/workbuddy2api` 路径。

### 1. 获取代码和配置

```bash
sudo git clone https://github.com/dddmiku/workbuddy2api.git /opt/workbuddy2api
cd /opt/workbuddy2api
sudo cp config.example.json config.json
```

编辑 `config.json`，将 `api_key` 的示例值改为自己的调用密钥。可以用以下命令生成随机值：

```bash
python3 -c 'import secrets; print(secrets.token_urlsafe(32))'
```

### 2. 准备数据目录并构建

镜像以 UID/GID `10001:10001` 运行。挂载目录需要允许该用户读写：

```bash
sudo install -d -o 10001 -g 10001 -m 700 auths data
sudo chown 10001:10001 config.json
sudo chmod 600 config.json
```

镜像已经发布在 ghcr.io，支持 `linux/amd64` 与 `linux/arm64`。不想在本机构建时，跳过下面的 `docker compose build`，改为拉取镜像：

```bash
sudo docker compose -f docker-compose.published.yml pull
```

需要固定版本时，把 `docker-compose.published.yml` 里的 `:latest` 换成[发布页](https://github.com/dddmiku/workbuddy2api/releases)上的版本号，例如 `:v1.1.0`。断网或 NAS 环境可以改用 Release 附件里的 `wb2api-amd64.tar.gz`（网关）和 `wb2api-admin-amd64.tar.gz`（管理台）离线导入。走镜像部署时，本文后续所有 `docker compose <子命令>` 都加上 `-f docker-compose.published.yml`。

在本机构建则执行：

```bash
sudo docker compose build
```

### 3. 添加账号

在镜像中运行登录工具，无需在宿主机额外安装 Go：

```bash
sudo docker compose run --rm --entrypoint /bin/bash wb2api /app/login.sh --realm=cn
```

按提示在浏览器完成授权。账号文件保存在 `auths/`。国际版使用 `--realm=global`，其可用性需要用相应账号另行验证。

### 4. 启动服务

```bash
sudo docker compose up -d
curl -sS http://127.0.0.1:7863/healthz
curl -sS -o /dev/null -w '%{http_code}\n' http://127.0.0.1:7864/login   # 管理台登录页，应返回 200
```

用已发布镜像时把上面三条里的 `docker compose` 换成 `docker compose -f docker-compose.published.yml`，端口、卷和健康检查与源码部署相同。

默认只绑定宿主机回环地址：网关 `127.0.0.1:7863`、管理台 `127.0.0.1:7864`。远程访问应通过自己的 HTTPS 反向代理。

`docker compose up -d` 会同时启动 `wb2api` 与 `wb2api-admin` 两个容器。管理台首次启动后按需初始化管理员账号（没有可继承的旧口令时，随机初始密码写入 `panel-data/initial-password.txt`）：

```bash
sudo docker compose exec wb2api-admin python3 -c 'import sys; sys.path.insert(0,"/app"); import app; app.load_credentials()'
cat panel-data/initial-password.txt
```

管理台的反向代理、端口与独立部署方式见 [管理台部署](docs/panel.md)。

以后添加或调整账号文件后，执行 `sudo docker compose restart wb2api` 重新加载。账号目录不支持热加载。

## 客户端接入

| 配置项 | 值 |
|---|---|
| Base URL | `http://127.0.0.1:7863/v1`，远程部署替换为自己的服务地址 |
| API key | `config.json` 中设置的密钥，或管理台创建的密钥 |
| 模型 | 从 `/v1/models` 获取，例如 `cn:deepseek-v4.1-flash` |
| Codex 协议 | `responses` |

以下示例从 `WORKBUDDY_API_KEY` 环境变量读取调用密钥：

```bash
curl -sS http://127.0.0.1:7863/v1/models \
  -H "Authorization: Bearer $WORKBUDDY_API_KEY"

curl -sS http://127.0.0.1:7863/v1/responses \
  -H "Authorization: Bearer $WORKBUDDY_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"model":"cn:deepseek-v4.1-flash","input":"Reply with OK.","stream":false}'
```

**Codex 说明：**官方 CLI 的默认系统说明里有一句渠道归属声明会被上游判为未授权渠道，网关会自动把这句话断词并在同一账号上重发一次，客户端只需要填 Base URL 和 API key。完整配置与实测记录见 [Codex 接入](docs/codex.md)。

## API key 管理

Web 页面由本仓库 `panel/` 提供的管理台承载，随 Compose 一起启动，访问 `http://127.0.0.1:7864/admin/#keys`（经反向代理时前缀由自己决定）。

在 `config.json` 中启用持久化密钥库：

```json
{
  "api_keys_file": "./data/api_keys.json",
  "api_keys_socket": "./data/api_keys.sock"
}
```

把上述字段合并到已有配置后重启网关。管理台只通过本机 Unix socket 管理密钥，不经过普通调用密钥。

- 首次创建密钥库时，现有 `api_key` 自动迁移，原有客户端可以继续使用。
- 密钥库建立后以库内状态为准；修改 `api_key` 不会重置或绕过密钥库。
- 列表只展示掩码；新密钥的完整值仅在创建时显示。
- 禁用或删除立即生效；空密钥库拒绝所有普通 HTTP 鉴权。
- 每个密钥可以绑定模型白名单（例如只允许 `cn:deepseek-v4.1-flash`）。绑定后越界模型返回 `403 model_not_allowed`，`GET /v1/models` 也只列出可用模型；留空表示不限制。

配置和目录权限详见 [配置说明](docs/configuration.md)。

## 热更新

部署之后不必为了升级重新构建镜像。管理台「系统 → 版本与热更新」显示当前版本与远端最新版本，点「立即更新」即从 [发布页](https://github.com/dddmiku/workbuddy2api/releases) 下载对应架构的二进制，校验 `sha256` 摘要后完成切换。

切换过程不打断正在进行的对话：

1. 新实例先以继承的监听套接字启动并开始服务。
2. 旧实例随后停止接受新连接，继续把手上的请求（包含流式对话）跑完，最长等待 15 分钟。
3. 旧实例以退出码 `75` 结束，容器 PID 1 保持存活，容器不重建。

管理通道的 Unix socket 同时交接，管理台不需要重新连接。从 v1.3.1 及更早版本升级请使用 v1.3.4 或更新版本，细节见配置说明。

下载件落在 `data/updates/`，`current` 指针保证容器重启后仍是新版本。回滚只需删掉该指针并重启：

```bash
sudo rm -f data/updates/current
sudo docker compose restart wb2api
```

细节、配置项与前置条件见 [配置说明 → 热更新](docs/configuration.md#热更新)，管理台入口见 [管理台部署](docs/panel.md)。

## HTTP 接口

除 `/healthz` 外，下列接口使用 Bearer API key 鉴权。

| 方法 | 路径 | 用途 |
|---|---|---|
| GET | `/healthz` | 本地账号池健康状态 |
| GET | `/v1/models` | 模型列表 |
| POST | `/v1/chat/completions` | Chat Completions 请求 |
| POST | `/v1/responses` | Responses 请求 |
| GET | `/status` | 账号池状态 |
| GET | `/tasks` | 任务状态 |
| POST | `/tasks/{key}/run` | 手动运行任务 |
| GET | `/tasks/{key}/log` | 任务日志 |

密钥管理不暴露在 `7863` 的 `/keys` 路由上。详细支持范围和错误语义见 [接口兼容性](docs/compatibility.md)。

## 开发与测试

```bash
go test ./...
go vet ./...
go test -race ./internal/auth ./internal/upstream ./internal/scheduler ./internal/server ./internal/apikeys
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o wb2api ./cmd/server
```

`-race` 需要支持 cgo 的 Go 环境和 C 编译器。真实大请求诊断依赖本地样本，未提供时会跳过；仓库内的公开合成夹具可以直接运行，不依赖私人会话文件。

```text
cmd/                  网关及登录、积分、任务命令
internal/             请求转换、上游访问、账号池与密钥管理
panel/                随仓库发布的 Web 管理台（Python 标准库 + 单文件前端）
scripts/              辅助任务脚本
docs/                 配置和客户端文档
examples/             可复用的客户端说明
config.example.json   配置模板
```

## 已知限制

- 这是兼容网关，未完整复刻官方客户端的设备信号、请求上下文与网络行为。
- 上游可能拒绝特定模型、账号或客户端渠道；健康接口成功不代表任意请求都可用。
- Responses 为无状态转换，需要客户端重传历史；不支持的能力会明确返回错误。
- 图片预算按字节裁剪，可能省略当前仍需要的图片；它不等于模型上下文 token 上限。
- 定时任务依赖上游活动，活动变化后可能需要更新。开关及时间见配置模板。

## 许可证

[MIT](LICENSE)。上游版权归 Sliverkiss，配套管理台及其他依赖遵循各自许可证。使用上游服务时应遵守其账号授权和服务要求。
