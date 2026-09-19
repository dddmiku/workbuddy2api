# 管理台部署

管理台源码在 `panel/`，与网关同仓库、同 Compose 项目发布。它管理上游账号、API key、排程任务、请求日志与用量统计，必须和网关运行在同一台机器上。

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
- 只有凭证文件确实不存在时才初始化。文件损坏或结构不合法会明确失败并保留原件，避免覆盖已有管理员资料。

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

Cookie 路径为 `/`，同时兼容根路径与 `/admin/` 部署；发放或清除 Cookie 时会清理旧的 `/admin/` 路径版本。上述 `/admin/` 配置的 `proxy_pass` 结尾斜杠不能省。HTTPS 反向代理必须设置可信的 `X-Forwarded-Proto`，面板据此添加 `Secure`。站点前面还有 Cloudflare 之类的代理时，按实际来源配置 Nginx `real_ip` 信任网段，否则登录限流会把所有访客算作同一个 IP。

退出会持久化撤销当前会话；重启面板后仍有效。滑动续期沿用同一会话标识，退出也会撤销该标识的旧副本。退出接口失败时页面会显示错误，不会把跳回登录页冒充退出成功。升级前已经产生的独立旧会话标识无法追溯关联；如需一次性撤销所有旧设备登录，可修改管理员密码。

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
| 查看请求日志 | 请求日志页，带表头（密钥 / 账号 / TTFB / token），可开自动刷新 |
| 查看 token 用量 | 用量统计页，总量卡片 + 按密钥明细 + 单密钥模型拆分 |
| 重启网关 | 系统页 → 重启服务 |

## 请求日志与用量统计

网关每完成一次请求就往 stdout 打一行表格日志，形如：

```text
| #012 | 22:04:21 | global:deepseek-v4.1-flash | stream | 200 | key=团队 A | uid=1e04e34d | TTFB=3414ms | in=306401 | hit=298112 | tok=110 | 34.3tok/s | total=3.4s |
```

`key=` 是本次请求使用的 API key 名称（无名称时回落掩码），因此一条日志就能看出是哪个调用方在用网关。`in=/hit=/tok=` 分别是一次请求的输入、其中命中提示缓存的输入、输出 token 数（含思考 token），全部取上游 `usage` 原值，缺失显示 `-`。

请求日志页把这类行解析成表格（含输入 / 缓存命中 / 输出三列，悬浮显示原值），行数可在 60/120/300/600 之间切换，「自动刷新」打开后每 5 秒拉一次（只在页面可见时拉）。不匹配的行（启动信息、WARN/ERR）折叠在页面底部的详情里。

新请求记录完整模型名，旧记录被截断的名称无法回填。「按密钥」和「请求日志」表头在页面向下滚动时固定，横向滚动保持列对齐；窄屏保留表格并允许左右滑动。切页或表格滚出视口后，固定表头自动消失。

用量统计页读取网关账本：每个 key 的请求数、输入 / 缓存命中 / 输出 / 合计 token、最近使用时间，以及该 key 的按模型拆分。账本默认落在密钥库同目录的 `usage.json`，每 5 秒或在进程退出时原子落盘。每个真正发起上游调用且已结束的客户端请求记录一次；鉴权失败和本地参数校验拒绝不计入。成功、失败、取消和输出约束拒绝都保留已经收到的原始用量；内部换号或路径重试会合计各次已知用量，但请求数不重复累加。

`failed_requests` 是最终错误或取消的请求数；正常 length/content_filter 不完整结果保留其独立语义。`unreported_requests` 表示至少有一次尝试未返回有效的完整输入/输出用量：显式零是已上报，缺失字段才是未知。页面会提示这类请求，累计值仅包含已确认部分，不能把它当成全部实际消耗。旧账本没有这两个状态字段，兼容读取为零仅代表此前没有记录，不能据此认定历史全部成功。

页面顶部有日期筛选：`今天` / `近 7 天` / `近 30 天` / `全部` 四个快捷区间，加上起止日期与「选择某一天」两个精确控件，切换即时生效（数据已在内存里，不再打网关）。账本按天分桶（键是服务端本地日历日 `YYYY-MM-DD`，与容器 `TZ` 一致，最多保留 120 天），所以「今天用了多少」是直接读当天桶，不是按时间戳反推。升级到带天桶的版本之前的历史只存在于总量里，按日期筛选时看不到它们——把筛选切回「全部」即可看到完整总量。

按日期筛选时，卡片和密钥表都只合计已有天桶，没有天桶就显示零。当前账本没有按天、按模型的交叉明细，因此模型拆分只在「全部」范围提供，不按比例推算。

账本只统计**经过本网关且已返回的原始用量**，同一台机器上直连其他服务商的流量不在内。每轮重发上下文会再次产生输入量，缓存命中已经包含在输入中，不能再相加一次；输出中的 `reasoning_tokens` 也是输出总数的明细，不重复相加。

Responses、Chat、日志与账本都采用上游原始观测口径，不乘输入估计倍率。账本与请求日志会按前述规则合计内部重试的已知用量；客户端最后一次响应与多次尝试的合计需要分别对应。提前压缩由客户端自己的窗口和阈值控制，设置方法见 [Codex 接入](codex.md#长会话的上限口径)。

旧 `server.input_token_scale` 或 `WB2A_INPUT_TOKEN_SCALE` 配置只兼容读取，启动告警后忽略。升级不会按旧倍率改算历史；缺少原始记录的历史不能从客户端估计、压缩结果或倍率反推出准确消耗。压缩成功与用量准确需要分别验证。

积分余额使用成功缓存优先、后台更新的方式加载。查询失败会结束“查询中”并显示错误；已有余额保留为上次结果，自动失败重试至少间隔 30 秒。手动刷新会加入正在执行的查询，防止并发结果互相覆盖。

## 版本与热更新

「系统」页的「版本与热更新」卡片显示当前版本、构建时间、远端最新版本、待下载资产与最近一次检查时间，并提供「检查更新」与「立即更新」。点「立即更新」后网关在后台下载并校验二进制，把监听套接字交给新实例，随后页面会每 2 秒轮询进度（空闲 / 检查中 / 下载中 / 切换中 / 失败）。

热更新先等待新实例完整就绪，再提交版本指针；候选实例失败时保留旧版本。旧进程为在途请求保留最多 15 分钟收尾时间，退出码 `75` 让容器 PID 1 继续监督新实例。超出收尾预算的请求仍可能中断。

入口脚本或管理台代码有变更时，需要更新完整镜像。常规容器重建与热更新的收尾机制不同，会有短暂服务窗口；网关常规 HTTP 收尾上限仍为 5 秒，Compose 的 3 分钟停止宽限还用于任务取消与状态落盘。重启操作只有在网关重新通过健康检查后才显示成功。

配置项、前置条件与回滚步骤见 [配置说明 → 热更新](configuration.md#热更新)。

## 面板内部接口

前端只调用下列路径，经反向代理访问时统一带 `/admin` 前缀；除登录外都需要会话 Cookie。

| 方法 | 路径 | 用途 |
|---|---|---|
| GET | `/login` | 登录页 |
| GET | `/`、`/index.html`、`/vendor/*` | 控制台资源，需登录 |
| GET | `/api/session` | 当前登录身份 |
| GET | `/api/state` | 账号状态；`refresh_credit=1` 触发积分刷新 |
| GET | `/api/models` | 网关模型目录（不受调用密钥的模型绑定限制） |
| GET | `/api/tasks`、`/api/task/log?key=` | 排程任务与单个任务日志 |
| GET | `/api/logs?lines=` | 网关容器日志（同时返回解析好的请求行 `rows`） |
| GET | `/api/usage` | 按 API key 累计的 token 用量（经本机 Unix socket 读网关 `/usage`） |
| GET | `/api/update` | 热更新状态（经本机 Unix socket 读网关 `/update`） |
| GET | `/api/keys` | 密钥列表 |
| POST | `/api/auth/login`、`/api/auth/logout`、`/api/auth/password` | 登录、退出、修改管理员账号 |
| POST | `/api/login/start`、`/api/login/poll` | 上游账号授权 |
| POST | `/api/account/toggle`、`/api/account/delete` | 账号开关、回收 |
| POST | `/api/task/run`、`/api/task/toggle` | 任务运行、开关 |
| POST | `/api/credit` | 刷新积分 |
| POST | `/api/service/restart` | 重启网关容器 |
| POST | `/api/keys`、`/api/keys/update`、`/api/keys/delete` | 密钥创建、修改（含 `models` 绑定）、删除 |
| POST | `/api/update/check`、`/api/update/apply` | 检查远端版本、触发一次热更新 |

所有 POST 接口（包括登录和退出）都需要 `Content-Type: application/json`、`X-Admin-Request: 1` 和有效的 JSON 对象；携带 `Origin` 时必须同源。管理动作还需要登录 Cookie。请求长度必须明确且合法，不接受分块请求体；密钥与热更新请求上限为 8 KiB，其他 POST 为 64 KiB。开关必须是 JSON 布尔值，不能用字符串 `"false"`。热更新的 `tag` 字段只允许字母、数字、点、下划线和短横线。

页面检查用 GET；后端未实现 HEAD，`curl -I` 的结果不能用来判断页面是否可用。

## 排障

| 现象 | 处理 |
|---|---|
| 页面返回 502 / 连接被拒绝 | `docker compose ps` 看 `wb2api-admin` 是否运行；`docker compose logs wb2api-admin` 看启动错误 |
| 密钥页提示未启用 | 网关 `config.json` 需要 `api_keys_file`，且 `data/api_keys.sock` 存在 |
| 添加账号后网关看不到 | 检查 `auths/` 文件属主是否为 `10001:10001`；面板会自行 chown，手工拷入的文件需自行处理 |
| 登录一直失败 | 连续失败 6 次会锁定 5 分钟；确认反向代理传递的 `X-Real-IP` 可信 |
| 忘记管理员密码 | 删除 `panel-data/credentials.json` 后重启容器，会重新继承 htpasswd 或生成新的初始密码 |

面板内部接口与更细的排障说明见 [panel/README.md](../panel/README.md)。
