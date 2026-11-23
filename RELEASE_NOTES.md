# Release Notes

## 0.1.0

首个版本：把 **OpenCode Go**（Go / Go Plus / Go Teams）订阅接入 CLIProxyAPI，并提供中文可视化控制台。

### Provider 引擎

- **按模型协议路由**：解析 `GET /inference/go/v1/models` 的 OpenAI list shape，读取每个模型的 `api` 字段（`messages` / `chat-completions` / `responses`）与 `api.base_url`，按声明的协议发送；`route-overrides` 可强制覆盖。对上游声明的 `base_url` 做 scheme/host 校验，防止凭据被重定向到任意主机。
- **三协议翻译**：OpenAI Chat Completions、Anthropic Messages、OpenAI Responses 的请求与流式响应统一互转；推理内容保留为 Claude `thinking` 块、Responses `reasoning` 项、OpenAI `reasoning_content`。
- **CPA 原生 Key 池**：每把 API Key 注册为一条 `opencodego` auth 记录，由 CPA 统一轮换、限流、冷却与会话亲和。客户端模型名默认用上游原名，可选 `opencode-go/` 前缀与逐条别名。

### 订阅额度

- **官方 `/v1/usage` 接口**：每把 Key 读取 `usage.rolling / weekly / monthly` 三个窗口的 `status`、`percent`、`resetsAt`，控制台展示剩余百分比与重置时间。
- **后台每分钟轮询**：插件进程内独立循环依次读取所有凭据并缓存；页面只读快照，开着自动跟随，关掉也继续刷新。列表接口不触达上游；单卡「刷新」立即强制读取。
- **失败退避与陈旧快照**：单账号失败不清空卡片，保留上次成功数据并标注原因，连续失败按 1/2/3… 分钟退避、上限 10 分钟。401/403 分别提示 Key 无效或无有效 Go 订阅。

### 控制台

- 概览、凭据、模型、额度、日志、统计、设置七个模块。
- 凭据 CRUD（新增 / 编辑 / 删除 / 启停 / 优先级）、模型发布白名单与别名映射、真实模型测试、请求日志搜索筛选、思考强度映射。
- 主题桥接与独立认证模块，复用 CPA 管理密钥登录，适配 `/v0`、`/v8` 挂载与反向代理前缀。

### 与直接配置上游 provider 的差异

- `api-keys` 可选，凭据可完全由页面管理。
- 新增 `data-dir` 与 Management API。
- 提供 `opencodego` provider key 与 auth 记录类型。
