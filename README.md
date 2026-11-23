# opencode-cpa-bridge

把 **OpenCode Go**（Go / Go Plus / Go Teams）订阅接入 [CLIProxyAPI（CPA）](https://github.com/router-for-me/CLIProxyAPI) 的原生动态库插件，并提供**中文可视化控制台**：凭据、模型目录、三窗口额度、请求日志与设置全部在网页里完成。

插件自带协议转换与调度内核：OpenAI Chat Completions / Anthropic Messages / OpenAI Responses 三种客户端协议，统一翻译到 OpenCode Go 声明的上游协议（每个模型在目录的 `api` 字段中声明 `messages` / `chat-completions` / `responses` 及其 `base_url`），响应与流式事件再转换回客户端协议。凭据以 CPA auth 文件形式注册，继续复用 CPA 原生的轮换、限流、冷却与会话亲和。

---

## 功能

### 控制台模块

| 模块 | 能力 |
| --- | --- |
| **概览** | 插件版本、已发布模型数、目录刷新时间与错误、数据目录、发布白名单/屏蔽数、一键刷新目录 |
| **凭据** | 页面内新增 / 编辑 / 删除 OpenCode Go API Key，启停账号，设置优先级，展示掩码密钥与标签 |
| **模型** | 查看已发现模型（客户端 ID、上游 ID、协议、上下文、是否支持思考），逐模型启用/禁用发布，**逐条编辑客户端模型名（别名映射）**，手动刷新目录，查看目录诊断，并**对任意模型发起真实测试** |
| **额度** | 每把 Key 的 5 小时滚动 / 每周 / 每月窗口：已用百分比、剩余百分比、状态（`ok` / `rate-limited`）、重置时间；插件后台**每分钟自动拉取** `/v1/usage` 并缓存，页面自动跟随（关掉页面也继续刷新），单卡可立即手动刷新 |
| **日志** | 请求日志（模型、来源协议、状态、耗时、首字耗时、tokens、错误）、搜索与状态筛选、分页、清空 |
| **统计** | 总请求、成功/失败、输入/输出/缓存 tokens、平均耗时，以及按模型聚合 |
| **设置** | Base URL、Catalog URL、目录刷新间隔、请求超时、最大响应字节、客户端模型前缀、三种协议开关、是否允许 http，以及**思考强度映射** |

页面设置会写入 `data-dir/settings.json`，覆盖 YAML 中的同名配置。

### Provider 引擎

- **动态目录发现**：默认 `GET {base-url}/models`，解析 OpenAI list shape 的 `data[]`，并把 OpenCode 的 `name`、`limit.context/output`、`modalities`、`capabilities.reasoning`、`api` 字段归一化。
- **按模型协议路由**：优先使用目录声明的 `api.messages`，其次 `chat-completions`、`responses`；每个模型的 `api.base_url` 单独生效（如 `https://opencode.ai/inference/go/anthropic/v1`），并做 host/scheme 安全校验。`route-overrides` 可强制覆盖。
- **推理保留**：上游 `reasoning` / `reasoning_details[].text` / `reasoning_content` 归一化后还原为 Claude `thinking` 块、Responses `reasoning` 项、OpenAI `reasoning_content`。
- **CPA 原生 Key 池**：每个凭据注册为一条 auth 记录，由 CPA 统一负责轮换、限流、冷却与会话亲和。
- **reasoning_effort 透传与映射**：未命中规则时原样透传；可在页面按模型配置改写或 `omit` 移除。

### 额度口径

OpenCode Go 官方用量接口：

```bash
curl https://opencode.ai/inference/go/v1/usage \
  -H "Authorization: Bearer <GO_API_KEY>"
```

```json
{
  "usage": {
    "rolling": { "status": "ok",           "percent": 30,  "resetsAt": "2026-11-21T15:00:00.000Z" },
    "weekly":  { "status": "rate-limited", "percent": 100, "resetsAt": "2026-11-23T00:00:00.000Z" },
    "monthly": { "status": "ok",           "percent": 50,  "resetsAt": "2026-12-01T00:00:00.000Z" }
  }
}
```

- `rolling` 为 5 小时窗口（月额度的 20%），`weekly` 每周一 00:00 UTC 重置，`monthly` 随订阅周期重置。
- `percent` 是**已用**百分比；控制台展示**剩余**百分比。
- `status: "rate-limited"` 或 `percent >= 100` 视为该窗口已触顶。
- HTTP 401 表示 Key 无效，403 表示无有效 Go 订阅；失败时保留上次成功快照并标记 stale，按退避重试（上限 10 分钟）。

---

## 安装

1. 让 CPA 启用插件：

   ```yaml
   plugins:
     enabled: true
     dir: /data/plugins
   ```

2. 将对应系统/架构的产物放入 `<cpa>/plugins/<os>/<arch>/`，文件名保持 `opencode-cpa-bridge`：

   | 平台 | 文件 |
   | --- | --- |
   | Linux amd64 / arm64 | `opencode-cpa-bridge.so` |
   | macOS amd64 / arm64 | `opencode-cpa-bridge.dylib` |
   | Windows amd64 / arm64 | `opencode-cpa-bridge.dll` |

3. **不需要在 CPA 里声明这个插件**。所有设置都在控制台页面里改，并落到 `data-dir`（默认 `plugins/opencode-cpa-bridge-data`，相对 CPA 进程工作目录）。仅当要覆盖默认值时才写：

   ```yaml
   plugins:
     enabled: true
     dir: /data/plugins
     configs:
       opencode-cpa-bridge:
         data-dir: "/data/plugins/opencode-cpa-bridge-data"
         base-url: "https://opencode.ai/inference/go/v1"
         api-keys:
           - value: "${OPENCODE_GO_API_KEY}"
   ```

4. 打开控制台：`/v0/resource/plugins/opencode-cpa-bridge/console`

   首次进入若提示密钥，请填 **CPA 管理密钥**。勾选「在此浏览器记住」后下次自动进入；已登录 CPA 管理面板时也会尝试复用其登录信息。

5. 在「凭据」页添加 OpenCode Go API Key，然后在「模型」页点击「刷新目录」。

---

## 配置项

| 配置项 | 类型 | 默认 | 说明 |
| --- | --- | --- | --- |
| `data-dir` | string | `plugins/opencode-cpa-bridge-data` | 页面设置与请求日志目录（也接受 `data_dir` 写法） |
| `base-url` | string | `https://opencode.ai/inference/go/v1` | OpenCode Go 推理地址（https；http 需 `allow-http`） |
| `catalog-url` | string | `{base-url}/models` | 目录发现地址 |
| `model-prefix.enabled` / `.value` | bool / string | `false` / `opencode-go` | 可选的客户端模型名前缀；默认关闭，逐条别名在「模型管理」页编辑 |
| `api-keys[].value` | string | *(空)* | 静态密钥，支持 `${ENV_VAR}`；留空则全由页面「凭据」管理 |
| `catalog.refresh-interval` | duration | `15m` | 目录轮询间隔（最小 `1m`） |
| `catalog.stale-while-unavailable` | bool | `true` | 刷新失败时继续提供上次快照 |
| `protocols.*` | bool | `true` | 协议开关；关闭即从目录排除对应路由的模型 |
| `request-timeout` | duration | `5m` | 上游请求超时 |
| `max-response-bytes` | int | `67108864` | 非流式响应体上限 |
| `allow-http` | bool | `false` | 允许 http:// 上游（仅本地测试） |

页面「设置」中保存的值作为覆盖层优先生效（`base-url` 变更时自动重算 `catalog-url`）。

---

## 调用示例

```bash
curl http://127.0.0.1:8317/v1/chat/completions \
  -H "Authorization: Bearer <CPA_API_KEY>" \
  -H "Content-Type: application/json" \
  -d '{"model":"kimi-k3","stream":true,"messages":[{"role":"user","content":"你好"}]}'
```

- 模型 `model` 取自「模型」页的**客户端模型 ID**（开启前缀时为 `opencode-go/<id>`）。
- Anthropic 客户端用 `/v1/messages`，Responses 客户端用 `/v1/responses`，插件自动转换。
- 关闭某协议后，对应路由的模型不会出现在 `/v1/models`。

---

## 数据与安全

- **凭据**：保存在 CPA 的 auth 目录（`opencodego-key-<sha256>.json`），密钥明文仅存于文件与上游请求头；页面只展示掩码。
- **设置 / 日志**：保存在 `data-dir`（`settings.json`、`requests.json`）。
- 容器部署时请持久化 CPA 的 auth 目录与插件的 `data-dir`。
- 目录声明的 `api.base_url` 会做 scheme/host 校验，防止上游数据把带凭据的请求重定向到任意主机。

---

## 从源码构建

需要 Go 1.26+ 与 CGO 工具链（gcc/clang）。

```bash
# Linux
CGO_ENABLED=1 go build -buildmode=c-shared -o opencode-cpa-bridge.so .
# macOS
CGO_ENABLED=1 go build -buildmode=c-shared -o opencode-cpa-bridge.dylib .
# Windows (mingw-w64)
set CGO_ENABLED=1
go build -buildmode=c-shared -o opencode-cpa-bridge.dll .
```

测试：

```bash
go test ./...
go vet ./...
```

---

## 项目结构

```
main.go                        CGO 胶水：导出 cliproxy_plugin_* 并转发到 internal/bridge
internal/
  bridge/                      插件核心
    plugin.go                   方法分发、生命周期、目录装载、Key 解析
    management.go               控制台资源页 + Management API
    credentials.go              凭据 CRUD（基于 CPA auth 文件）
    settings.go                 页面设置持久化与发布过滤
    quota.go                    /v1/usage 三窗口额度查询与后台每分钟轮询
    hostbridge.go               宿主回调桥（HTTP / 日志 / auth / 流）
    executor.go                 请求执行（流式 / 非流式，按模型 base_url 路由）
    reasoning_mapping.go        思考强度映射
    modeltest.go                模型测试
    telemetry.go                请求日志与统计
  translate/
    openai/                     OpenAI Chat Completions 适配
    anthropic/                  Anthropic Messages 适配
    responses/                  OpenAI Responses 适配
    common/                     共享翻译内核与推理处理
  registry/                     OpenCode Go 目录发现、协议路由与安全校验
  reasoning/                    思考预算与强度能力
  fault/                        错误分类与信封
  config/                       配置加载、校验与页面覆盖层
resources/index.html            中文单页控制台
resources/auth.js               控制台认证模块（占位注入）
```

---

## 许可与来源

本项目采用 MIT 许可。协议转换与调度内核经改写，源自 [command-code-cpa-bridge](https://github.com/xurane86/command-code-cpa-bridge)（MIT）与 [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI)（MIT）；控制台的「页面插件」形态参考 [ClinePassBridge](https://github.com/xiao-qiu-qiu/ClinePassBridge)（MIT）。按 MIT 许可要求保留原始版权声明，详见 [LICENSE](LICENSE) 与 [NOTICE](NOTICE)。
