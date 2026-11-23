# OpenCode Go API 参考

本文件记录插件依赖的 OpenCode Go 公开接口形状（2026-11-23 依据官方文档核对）。这些接口没有 Go  SDK 绑定保证，上游可能调整，插件对所有字段都做容错解析。

## 端点

| 用途 | 方法与路径 | 认证 |
| --- | --- | --- |
| 模型目录 | `GET https://opencode.ai/inference/go/v1/models` | 可选（带 Key 时返回该工作区可用模型） |
| 推理 | `POST {api.base_url}/{chat/completions\|messages\|responses}` | `Authorization: Bearer <GO_API_KEY>` |
| 订阅用量 | `GET https://opencode.ai/inference/go/v1/usage` | `Authorization: Bearer <GO_API_KEY>` |
| Console 模型列表 | `GET https://opencode.ai/console/api/v1/models` | 需个人 Key 或浏览器会话 |

插件默认 `base-url = https://opencode.ai/inference/go/v1`，目录地址为 `{base-url}/models`，用量地址由 `base-url` 去掉末尾 `/v1` 后拼 `/v1/usage` 得到，因此 staging / 自托管域名只要保持相同路径结构即可工作。

## 模型目录

推理目录返回 OpenAI list shape：

```json
{
  "object": "list",
  "data": [
    {
      "id": "kimi-k3",
      "object": "model",
      "name": "Kimi K3",
      "limit": { "context": 1000000, "output": 128000 },
      "modalities": { "input": ["text", "image/png"], "output": ["text"] },
      "capabilities": {
        "tools": { "supported": true },
        "reasoning": { "supported": true, "effort": ["low", "medium", "high"] }
      },
      "api": {
        "messages": {
          "base_url": "https://opencode.ai/inference/go/anthropic/v1",
          "sdk": { "aisdk": "@ai-sdk/anthropic" }
        }
      }
    }
  ]
}
```

插件的归一化规则：

| 上游字段 | 归一化到 | 备注 |
| --- | --- | --- |
| `id` | UpstreamID | 必填，缺失则排除并记入诊断 |
| `name` / `display_name` | DisplayName | 为空时回退到 `id` |
| `limit.context` / `context_length` | ContextLimit | 上下文窗口 |
| `limit.output` / `max_output_tokens` | OutputLimit | 输出上限 |
| `modalities.input` / `input_modalities` | InputModes | 输入模态 |
| `modalities.output` / `output_modalities` | OutputModes | 输出模态 |
| `capabilities.reasoning` | Thinking | `supported=true` 时生成 `ThinkingSupport`，`effort` 映射为 levels |
| `api` | Route + UpstreamBaseURL | 见下 |

### 协议路由优先级

1. `route-overrides.<model-id>`：用户显式指定 `protocol` 与 `endpoint`。
2. `api.messages` → Anthropic Messages，端点 `/v1/messages`。
3. `api["chat-completions"]` → OpenAI Chat Completions，端点 `/v1/chat/completions`。
4. `api.responses` → OpenAI Responses，端点 `/v1/responses`。
5. 目录未声明 `api`：回退到 Chat Completions（兼容旧目录）。
6. 目录声明了 `api` 但只有不支持的协议（如 `generate-content`、`embeddings`）：排除该模型并记入诊断。

`api.<protocol>.base_url` 作为该模型的独立上游地址。插件校验其必须为绝对 URL、scheme 为 `https`（`allow-http` 时允许 `http`）、无 query/fragment/userinfo，并校验拼接后的端点不逃逸原 host。

## 推理响应

Go 响应在 `opencode.allowance` 中报告本次请求计入订阅额度的金额。插件当前不把它折算为绝对额度条（`/v1/usage` 只提供百分比），但会照常透传响应体给客户端。

Token 使用量照常从各协议的标准 `usage` 字段读取，写入请求日志与统计。

## 订阅用量

```json
{
  "usage": {
    "rolling": { "status": "ok",            "percent": 30,  "resetsAt": "2026-11-21T15:00:00.000Z" },
    "weekly":  { "status": "rate-limited",  "percent": 100, "resetsAt": "2026-11-23T00:00:00.000Z" },
    "monthly": { "status": "ok",            "percent": 50,  "resetsAt": "2026-12-01T00:00:00.000Z" }
  }
}
```

| 字段 | 含义 |
| --- | --- |
| `status` | `ok` 或 `rate-limited`（额度用尽） |
| `percent` | **已用**百分比，整数 0–100；插件展示剩余百分比 |
| `resetsAt` | ISO 8601 重置时间；5 小时窗口尚未开始时为「当前时间 + 5 小时」 |

窗口口径：

- `rolling`：5 小时窗口，额度为月额度的 20%，从首次使用开始计时。
- `weekly`：每周窗口，为月额度的 50%，每周一 00:00 UTC 重置。
- `monthly`：订阅月额度，随新计费周期重置。

错误状态：

| HTTP | 含义 | 插件处理 |
| --- | --- | --- |
| 401 | Key 无效或已撤销 | 卡片显示 401 提示，进入退避 |
| 403 | Key 无有效 Go 订阅（`EntitlementError`） | 卡片显示 403 提示，进入退避 |
| 5xx / 超时 | 上游不可用 | 保留上次成功快照并标记 stale |

## 与 Console 模型的区别

`GET /console/api/v1/models` 返回以 provider ID 为键的对象（`opencode`、`opencode-go`、`console-<slug>`），包含工作区的连接与编辑；其模型条目使用 `canonical_id`、`cost`、`api` 等更丰富的字段。插件不使用该接口做路由发现，因为它需要工作区级权限且包含用户私有连接；`opencode-go` provider ID 仅用于文档对照，插件自身的 auth provider ID 为 `opencodego`。
