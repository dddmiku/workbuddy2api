# workbuddy2api-panel

`workbuddy2api` 的 Web 管理台源码，随网关同一个仓库发布。管理账号、API key、排程任务与容器日志。

后端只用 Python 标准库，前端由 `src/` 与 `app.js` / `keys.js` 拼成单文件 `index.html`，不需要 Node 构建链。

## 与网关一起部署（推荐）

仓库根目录的 `docker-compose.yml` 已经包含 `wb2api-admin` 服务：

```bash
cd /opt/workbuddy2api
sudo docker compose up -d --build
```

面板容器通过宿主 `docker.sock` 控制网关容器，并把仓库目录挂到 `/gateway`，共享 `config.json`、`auths/`、`data/`。面板自身凭证写入 `panel-data/`（不进版本库）。

首次启动后按需初始化管理员：

```bash
sudo docker compose exec wb2api-admin python3 -c 'import sys; sys.path.insert(0,"/app"); import app; app.load_credentials()'
```

没有可继承的旧口令时，随机初始密码写入 `panel-data/initial-password.txt`。

## 独立运行（可选）

面板也可以直接在宿主机跑（systemd 或前台进程），此时用环境变量指向网关目录：

```bash
WB2API_GATEWAY_DIR=/opt/workbuddy2api \
WB2API_ADMIN_DIR=/opt/wb2api-admin \
WB2API_GATEWAY_URL=http://127.0.0.1:7863 \
python3 panel/app.py
```

| 环境变量 | 默认值 | 说明 |
|---|---|---|
| `WB2API_GATEWAY_DIR` | `/opt/workbuddy2api` | 网关目录，读取 `config.json` 并管理 `auths/`、`data/` |
| `WB2API_ADMIN_DIR` | 脚本所在目录 | 面板凭证与初始密码文件目录 |
| `WB2API_ADMIN_HOST` | `127.0.0.1` | 监听地址；容器内为 `0.0.0.0` |
| `WB2API_ADMIN_PORT` | `7864` | 监听端口 |
| `WB2API_GATEWAY_URL` | `http://127.0.0.1:7863` | 网关 HTTP 地址 |
| `WB2API_CONTAINER` | `workbuddy2api` | 网关容器名（docker CLI 操作对象） |
| `WB2API_HTPASSWD_PATH` | `/etc/nginx/.htpasswd_wb2admin` | 可选的旧口令继承文件 |

## 开发

```bash
python3 panel/build.py          # 重新生成 index.html
python3 -m unittest discover -s panel -p 'test_*.py' -v
```

改版式动 `panel/src/*.css`、`panel/src/body.html`；改行为动 `panel/app.js`、`panel/keys.js`。

完整部署、反向代理与排障说明见仓库 [README](../README.md) 与 [docs/panel.md](../docs/panel.md)。
