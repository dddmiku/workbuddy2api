# 管理台部署

管理台源码在 `panel/`，与网关同仓库、同 Compose 项目发布。它管理上游账号、API key、排程任务和容器日志，必须和网关运行在同一台机器上。

## Compose 部署

仓库根目录的 `docker-compose.yml` 已包含 `wb2api-admin` 服务，`docker compose up -d` 会与网关一起启动：

| 项目 | 网关 | 管理台 |
|---|---|---|
| 容器名 | `workbuddy2api` | `wb2api-admin` |
| 宿主端口 | `127.0.0.1:7863` | `127.0.0.1:7864` |

管理台容器挂载了三处：

- `/var/run/docker.sock`：用于重启网关容器、读取日志、执行镜像内的登录与积分工具。
- `./:/gateway`：读写 `config.json`，管理 `auths/`、`auths-trash/`，连接 `data/api_keys.sock`。
- `./panel-data:/data`：保存管理台自身的 `credentials.json` 与初始密码提示。

容器以 root 运行，因为需要访问宿主 docker socket 并改写网关目录中的文件；它只映射到宿主回环地址，不直接暴露公网。

### 首次初始化

```bash
sudo docker compose exec wb2api-admin python3 -c 'import sys; sys.path.insert(0,"/app"); import app; app.load_credentials()'
```

- 存在可继承的 `/etc/nginx/.htpasswd_wb2admin` 时沿用其用户名与 APR1 口令。
- 否则生成 `wbadmin` 账号，随机初始密码写入 `panel-data/initial-password.txt`。
- 登录后可在线修改用户名与密码；改密会轮换会话密钥，其他设备上的登录立即失效。

## 反向代理

管理台监听回环地址，公网访问必须经自己的 HTTPS 反向代理，例如把 `/admin/` 指到面板：

```nginx
location /admin/ {
    proxy_pass http://127.0.0.1:7864/;
    proxy_http_version 1.1;
    proxy_set_header Host $http_host;
    proxy_set_header X-Real-IP $remote_addr;
    proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
    proxy_set_header X-Forwarded-Proto $scheme;
    proxy_read_timeout 600s;   # 扫码添加账号、重启容器等操作耗时较长
}
```

Cookie 路径固定为 `/admin/`，`proxy_pass` 结尾的斜杠不能省。站点前面还有 Cloudflare 之类的代理时，按实际来源配置 Nginx `real_ip` 信任网段，否则登录限流会把所有访客算作同一个 IP。

## 环境变量

容器模式已内置下表默认值；在宿主机直接运行时用同样变量指向自己的目录。

| 变量 | 容器默认 | 说明 |
|---|---|---|
| `WB2API_GATEWAY_DIR` | `/gateway` | 网关目录（`config.json`、`auths/`、`data/`） |
| `WB2API_ADMIN_DIR` | `/data` | 管理台凭证目录 |
| `WB2API_ADMIN_HOST` | `0.0.0.0` | 监听地址 |
| `WB2API_ADMIN_PORT` | `7864` | 监听端口 |
| `WB2API_GATEWAY_URL` | `http://wb2api:7863` | 网关 HTTP 地址 |
| `WB2API_CONTAINER` | `workbuddy2api` | 网关容器名 |
| `WB2API_HTPASSWD_PATH` | `/etc/nginx/.htpasswd_wb2admin` | 旧口令继承文件（可选） |

在宿主机直接运行（systemd 或前台进程）时，`WB2API_ADMIN_DIR` 建议设为 `/opt/wb2api-admin`，并把 `WB2API_GATEWAY_DIR` 指向 `/opt/workbuddy2api`。

## 常用操作

| 操作 | 位置 |
|---|---|
| 添加账号 | 账号页 → 添加账号，扫码后在 App 内确认 |
| 启用 / 停用账号 | 账号列表，改完自动重启网关容器 |
| 创建 API key | 密钥页 → 创建密钥，可绑定模型白名单 |
| 调整定时任务 | 任务页开关，改完重启网关生效 |
| 查看容器日志 | 系统页 → 容器日志 |
| 重启网关 | 系统页 → 重启服务 |

## 排障

| 现象 | 处理 |
|---|---|
| 页面返回 502 / 连接被拒绝 | `docker compose ps` 看 `wb2api-admin` 是否运行；`docker compose logs wb2api-admin` 看启动错误 |
| 密钥页提示未启用 | 网关 `config.json` 需要 `api_keys_file`，且 `data/api_keys.sock` 存在 |
| 添加账号后网关看不到 | 检查 `auths/` 文件属主是否为 `10001:10001`；面板会自行 chown，手工拷入的文件需自行处理 |
| 登录一直失败 | 连续失败 6 次会锁定 5 分钟；确认反向代理传递的 `X-Real-IP` 可信 |
| 忘记管理员密码 | 删除 `panel-data/credentials.json` 后重启容器，会重新继承 htpasswd 或生成新的初始密码 |

面板内部接口与更细的排障说明见 [panel/README.md](../panel/README.md)。
