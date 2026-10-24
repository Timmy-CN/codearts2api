# CodeArts2API

> 华为云 CodeArts Agent（盘古助手/码道）的 OpenAI 兼容代理。**无需运行 CodeArts Agent
> 客户端**，纯 Go 直连华为云 API，多账号轮转 + token 自动续期 + WebUI。

## 参考项目

本项目是 [Sliverkiss](https://github.com/Sliverkiss) 同系列开源项目的延伸实现，架构与运维形态参考了以下仓库：

- [workbuddy2api](https://github.com/Sliverkiss/workbuddy2api) — WorkBuddy CN OpenAI 兼容反代（账号池 / 轮转 / 签到架构）
- [traework2api](https://github.com/Sliverkiss/traework2api) — TRAE Work OpenAI 兼容反代（零依赖 Go 骨架）
- [qoderwork2api](https://github.com/Sliverkiss/qoderwork2api) — QoderWork CN OpenAI 兼容反代（OAuth 授权流程）

感谢原作者的开源与优秀设计。

## 快速开始（Ubuntu / Linux）

```bash
make linux            # bin/ 下 4 个 Linux 静态二进制
make test
```

### 登录（华为云账号）

```bash
# 本机有浏览器
./login.sh

# 服务器（无浏览器）：打印链接，任意机器浏览器打开，ticket 轮询下发
./login.sh -print-only

# 凭证落盘 auths/codearts-{user_id}.json
```

### 启动

```bash
cp config.example.json config.json
export CA2A_API_KEY=你的随机密钥
./bin/codearts2api -config config.json
```

### 验证 + WebUI

```bash
curl http://127.0.0.1:7866/healthz
curl http://127.0.0.1:7866/v1/models -H "Authorization: Bearer $CA2A_API_KEY"
curl -X POST http://127.0.0.1:7866/v1/chat/completions \
  -H "Authorization: Bearer $CA2A_API_KEY" -H "Content-Type: application/json" \
  -d '{"model":"snap-chat","messages":[{"role":"user","content":"你好"}]}'
```

浏览器打开 **http://127.0.0.1:7866/** 即 WebUI：账号/token 状态、对话测试（流式/非流式）。多轮上下文按账号自动续接（chat_id 分组）；也可用请求头
`X-Codearts-Chat-Id: <chatId>` 或 body 里 `conversation_id` 显式指定会话。

流式请求设置 `"stream_options":{"include_usage":true}` 后，会在 `[DONE]` 前返回一次
`choices: []` 的 usage chunk；其余 chunk 的 `usage` 为 `null`。未启用时不返回 usage。
流式和非流式均优先使用上游 usage；上游未提供时按文本长度估算，估算值不是精确 token 计数。

工具调用支持 `tool_choice` 的 `auto` / `none` / `required` 及指定函数对象。
指定函数时，网关只向上游发送该函数的定义，并转换为上游支持的字符串 `required`；
指定函数不存在或重复定义时返回 400。上游 `InferHub.001001005` 参数错误也返回 400，
不轮换、不冷却账号；若流式响应已开始，则用 SSE 错误事件结束，同样不处罚账号。

### 模型列表与限时福利

`/v1/models` 返回上游**精确模型 ID**（区分大小写，如 `GLM-5.2`、`Qwen3-VL-235B`），
同时为含大写的 ID 补一条小写别名（`glm-5.2`），两者都能用于聊天。限时福利
（免费套餐）模型额外带 `benefit: true` 标记，聊天时服务端会自动追加上游要求的
`maas_type: benefit` 请求头（按发起请求的账号判定，多账号套餐不同也不会串——
列表是各账号可用模型的并集，实际路由会优先挑目录里真有这个模型的账号）。

福利模型列表随免费套餐轮换，用下面这条命令核对当前账号实际可用的模型：

```bash
go run ./cmd/models                 # 账号可用模型（内置 + 福利）
go run ./cmd/models -json           # 机器可读
go run ./cmd/models -claim          # 先领取限时福利再查询（幂等，属写操作）
```

限时福利模型**必须先领取才能调用**：未领取时上游一律返回
`InferHub.4004.200 benefit not found`（实测）。领取是幂等操作，官方客户端打开模型
菜单时也会调用，因此服务默认会领取（`benefit_auto_claim: true`）。不想让服务写账号
可显式关闭：

```jsonc
"benefit_auto_claim": false   // 或 CA2A_BENEFIT_AUTO_CLAIM=0
```

关闭后福利模型仍会出现在 `/v1/models`，但调用必然失败。

领取覆盖三处（都按北京日幂等）：模型发现时（上节）、watchdog 每轮对每个启用
账号补领、以及聊天请求被 `InferHub.4004/4005 benefit not found` 拒绝时先补领
一次再重发同一账号（跨北京日/领取过期的兜底）。

### 模型可用性探测

`/v1/models` 只列**真实可用**的模型：上游的模型发现（agent-center / builtin / 福利
网关）会列出当前账号根本没注册的模型（实测 `GLM-5.2-ArkTS-SPARK`、`OpenPangu-2.0-Pro`
等返回 `InferHub.002002009.404`）。服务会周期性用一条最小请求探测并缓存结论，
真实请求失败也会被学习（`not registered` / `benefit not found` 记为账号能力问题，
不会给健康账号记错误冷却）。探测结果缓存 30 分钟。

`-vl` 视觉模型（如 `Qwen3-VL-235B`、`kimi-k2.6-vl`）在三个发现来源里统一隐藏：
上下文小、无 tool call，官方 IDE 与插件都不把它们作为 agent 主模型。

### API Key 必填

服务没有 API Key 会**拒绝启动**（旧版本会回退到公开的 `dummy-key-for-codearts`，
等于把接口暴露给任何人）。三选一：

```bash
# 1) env（推荐，配合 .env / systemd EnvironmentFile）
export CA2A_API_KEY=$(openssl rand -hex 24)
# 2) config.json 的 "api_key" 字段
# 3) docker compose 的 .env
```

### 账号续期

`refresh_token` 与登录时的 `client_id`、DPoP 私钥绑定，因此凭证文件会保存
`client_id` 与 `dpop_private_key`；刷新时原样复用并写回轮转后的新 refresh_token。
若手工换过 `login_client_id`，老账号仍按自己记录的 client_id 刷新。

refresh_token 是一次性的：若它已被 `cmd/apply`、`cmd/login` 或另一个实例换新，
本进程刷新会撞「has been used」——此时服务会重读磁盘上的凭证文件并改用最新的
refresh_token 重试一次，而不是直接禁用账号。

### 上游请求行为对齐（与官方客户端/插件同口径）

- **网关排队**：上游 `429 + queue-status: queued + trace-id` 时，按 `retry-after`
  轮询 `concurrency/trace-queues/{id}` 直到 `promoted` 再重发；预算 300s、连续
  3 次轮询失败或被移出队列则 DELETE 退出并回 429。
- **thinking 注入**：请求未显式给 `thinking` 时注入 `{"type":"enabled"}`（思考链
  才会进 `reasoning_content`）；被模型以参数错误拒绝（400 文案点名 thinking）时
  自动摘除重发一次。可用 `thinking: false` 显式关闭。
- **max_tokens 钳制**：超过 65536 一律钳到上限，避免上游拒绝。
- **不跟随重定向**：签名请求与令牌请求遇到 3xx 一律转 502，AK 签名头与
  security token 绝不随跳转外泄。
- **稳定会话头**：每个账号的聊天共用一个由 user_id 派生的稳定
  `x-ot-session-id`（`user-session-id` 同值，`x-ot-trace-id`/`x-ot-span-id`
  每次随机）——网关按这个头统计单账号 3 个并发会话，共用会话 id 才不会被
  槽位 ~1 分钟的释放滞后顶满（插件实测：不带此头的请求各自占一个新会话）。
  因此 `max_concurrent` 可以按需调高而不再默认撞 `TM.00001041`。

### 每日签到与新手积分

服务默认在启动和每次 watchdog 轮询时检查活动，自动领取每日签到积分及
上游返回的可领取新用户积分。新增账号会在下一次轮询处理；也可在 WebUI
点击「签到」/「全员签到」，或鉴权调用 `POST /admin/api/checkin`（可传 `{"uid":"账号 ID"}`）。
领取后必须确认；已领取未确认时只补确认，已确认不重复领取。资格、金额、
每日刷新和活动起止时间均以上游返回为准，失败会在下一次轮询重试，
不会因活动接口故障冷却或禁用推理账号。

`credits_auto_claim: false` 或 `CA2A_CREDITS_AUTO_CLAIM=0` 可关闭自动领取，
`watch.enabled: false` 会关闭整个轮询。手动签到仍可使用。
这项活动与 `benefit_auto_claim` 的模型 Token 福利独立；面板显示最近领取结果。
`credit.sh` 查登录态，`apply.sh` 仅续期。

### 积分余额查询

`POST /admin/api/credits`（面板「校验」按钮）在登录态校验之外返回真实积分快照：
`GET /snap-manager/v1/statistics/plugin` 的四个积分桶（总/基础/按需/赠送）、
`snap-manager/v1/package/credit/page` 与 `ops/credit` 的到期批次（月度重置 =
套餐批次最早到期；签到积分按各自到期时间列出）、opengw 的每日免费 token 池。
快照缓存 10 分钟，签到成功后强制重读；批次/token 池读不到时快照照常返回并在
`error` 字段说明。面板账号表新增「积分」列（来自最近一次查询的缓存）。

`/v1/models` 与面板模型目录带 `rate`（agent-center `credit[].ratio_display`
消耗系数）与 `reasoning`（`model_parameters.thinking_type` 存在）标记。

当前[官方每日签到活动](https://support.huaweicloud.com/offers-codeartsagent/codeartsagent_offers_0004.html)
为已升级积分计费的用户每日发放 1000 积分，30 天有效，活动截止于
2026-12-31 00:00（UTC+8）；[新用户注册奖励](https://support.huaweicloud.com/price-codeartsagent/codeartsagent_billing_0029.html)
为 4000 积分。无资格或已自动发放的新手奖励不会再次领取。

## 部署（systemd / Docker）

```bash
sudo mkdir -p /opt/codearts2api && sudo cp -r bin config.example.json auths /opt/codearts2api/
sudo cp deploy/codearts2api.service /etc/systemd/system/
# 编辑 /opt/codearts2api/.env 写 CA2A_API_KEY，改好 config.json
sudo systemctl daemon-reload && sudo systemctl enable --now codearts2api

# 或 Docker
export CA2A_API_KEY=你的随机密钥
mkdir -p auths data
docker compose up -d --build
```

## 环境变量配置

除了 `config.json`，还支持以下环境变量覆盖：

| 变量名 | 说明 | 默认值 |
|--------|------|--------|
| `CA2A_API_KEY` | API 访问密钥 | - |
| `CA2A_LISTEN` | 监听地址 | `:7866` |
| `CA2A_AUTH_DIR` | 凭证目录 | `./auths` |
| `CA2A_STATE_FILE` | 状态文件 | `./data/state.json` |
| `CA2A_DEFAULT_MODEL` | 默认模型 | `glm-5.2` |
| `CA2A_OAUTH_CALLBACK_HOST` | OAuth 回调主机 | - |
| `CA2A_WATCH_ENABLED` | 调度器开关 | `true` |
| `CA2A_WATCH_POLL_MINUTES` | 轮询间隔（分钟） | `30` |
| `CA2A_WATCH_REFRESH_SKEW` | 提前刷新时间（分钟） | `30` |
| `CA2A_WATCH_KEEPALIVE_INTERVAL` | 保活间隔（分钟） | `15` |
| `CA2A_MAX_CONCURRENT` | 单账号最大并发 | `5` |
| `CA2A_KEEPALIVE_WINDOW` | 保活窗口 | `10m` |
| `CA2A_BENEFIT_AUTO_CLAIM` | 发现模型时自动领取限时福利（幂等；关闭则福利模型不可用） | `true` |
| `CA2A_CREDITS_AUTO_CLAIM` | watchdog 自动领取每日签到及新用户积分 | `true` |
| `CA2A_LOGIN_CLIENT_ID` | WebUI 登录使用的 OAuth client_id | 已有账号的取值，否则 `codearts-agent` |

## 目录结构

```
cmd/server/        HTTP 服务（config + main）
cmd/login/         华为云 OAuth2 PKCE 登录
cmd/credit/        账号登录态日报（新增并发信息）
cmd/apply/         批量 token 续期（使用 pool 包）
cmd/models/        查看账号可用模型（内置 + 限时福利，可选 -claim 领取）
internal/auth/     auth 文件读写
internal/upstream/ 云端客户端（登录/聊天/SSE/模型发现）+ 逆向常量
internal/pool/     账号池（token 校验/自动刷新/冷却/并发控制）
internal/scheduler/ token 续期看门狗（新增保活机制）
internal/server/   OpenAI 兼容路由
internal/webui/    内嵌 WebUI 控制台
deploy/            systemd unit 样例
docs/              逆向过程与接口清单
```

## 免责声明

仅供学习和研究使用。使用者需遵守华为云服务条款，自行承担使用风险。

## License

MIT

