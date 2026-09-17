# 配置说明

配置从 `config.json` 加载。完整模板见 [config.example.json](../config.example.json)，默认值和校验逻辑见 `cmd/server/config.go`。

## 常用配置

| 字段 | 默认值 | 说明 |
|---|---|---|
| `listen` | `:7863` | 容器内监听地址；宿主机映射由 Compose 控制 |
| `api_key` | 空 | 单密钥模式的调用密钥；多密钥模式首次建库时用于迁移 |
| `api_keys_file` | 空 | 非空时启用持久化密钥库 |
| `api_keys_socket` | 密钥库同目录下 `api_keys.sock` | 多密钥管理通道 |
| `auth_dir` | `./auths` | 启动时加载 `workbuddy*.json` |
| `state_file` | `./data/state.json` | 账号池状态文件 |
| `server.max_body_mb` | `8` | 入站请求体上限，必须大于 0 |
| `server.outbound_image_budget_mb` | `7` | 出站图片字节预算；非正数关闭裁剪 |
| `prompt.mode` | `passthrough` | 保留客户端指令；`custom` 才执行显式替换 |
| `prompt.file` | 空 | `custom` 模式使用的提示词文件 |
| `pool.max_in_flight` | `3` | 单账号并发上限；0 表示不限制 |
| `session_sticky.enabled` | `true` | 按客户端会话键选择账号 |
| `global.enabled` | `true` | 是否允许国际版账号路由 |

配置模板中的示例值不等于安全的部署默认值。对外部署前必须设置调用密钥。

## 单密钥与多密钥

未配置 `api_keys_file` 时，`api_key` 为空会关闭普通 HTTP 鉴权。

配置 `api_keys_file` 后，密钥库负责鉴权：

1. 文件不存在时创建密钥库；非空 `api_key` 迁移为现有密钥。
2. 文件已存在时加载原有内容，不再次导入或覆盖。
3. 空库拒绝调用，不能通过清空 `api_key` 恢复免鉴权。
4. 配置文件损坏、密钥库不可读或 socket 不可用会阻止正常启动，应根据日志修复。

推荐布局：

```json
{
  "api_keys_file": "./data/api_keys.json",
  "api_keys_socket": "./data/api_keys.sock"
}
```

密钥库只保存摘要，不能找回完整密钥；丢失后创建新密钥并停用旧密钥。备份时同时保留网关配置、账号目录、状态和密钥库。

每把密钥可以在管理台绑定模型白名单：绑定后该密钥只能调用列出的模型，`GET /v1/models` 也只返回这些模型；留空表示不限制。绑定同时保存在密钥库里，重启后继续生效。

## Docker 目录权限

镜像用户是 `10001:10001`。`auths/` 与 `data/` 需要该用户读写，`config.json` 需要该用户可读。

新增账号必须重启加载。容器内运行登录脚本会按镜像用户写文件；若在宿主机导入账号，需要同步检查文件的所有者和 0600 读取权限。

管理台在宿主机使用同一个 `data/` 目录连接 socket。共享目录，避免单独挂载重启时会被替换的 socket 文件。socket 包含管理能力，不能公开反向代理。

## 定时任务

每个任务使用 `schedule.<key>_enabled` 开关和 `schedule.<key>_hours` 小时列表，小时按服务时区解释。Compose 默认时区为 `Asia/Shanghai`。

任务包括 `checkin`、`travel`、`activity`、`keepalive`、`school`、`cat`、`redeem`、`lottery`、`makeup`。关闭任务应设置对应开关为 `false`，空小时列表不会表示禁用。

同一实例中的任务执行使用互斥以隔离各自日志，同时到期的任务会排队执行。

活动任务依赖上游接口和开放状态。首次部署可先关闭不需要的任务，再逐项启用。

## 环境变量

服务支持部分 `WB2A_*` 覆盖项，具体列表以 `cmd/server/config.go` 的 `applyEnv` 为准。`api_keys_file`、`api_keys_socket` 当前通过 JSON 配置，不应假定所有字段都有对应环境变量。

`features.sanitize_blacklist_fingerprints` 仅保留旧配置兼容，已废弃，不再修改请求正文。
