# model-info

CPA 标准只读工作插件。在内存中保存 Writer 获取并验证后的 Codex client model catalog，向 Management Center 提供原始与 effective 视图。

`model-info` 不读取 CPA 配置/密钥文件，不持有 management 或 proxy credential，不发起 HTTP 请求，也不直接刷新 catalog。成功 reconfigure（包括仅 `sync_epoch` 变化）保留现有缓存；失败 ingest 同样保留缓存。

## 配置

ConfigYAML 严格接受 model-info 字段 `worker_token_env` 与可选 `sync_epoch`，以及 Core 注入/持有的 `enabled`、`priority`、`store`。`store` 仅作为不透明安装身份保留：插件不读取其字段，但递归拒绝 alias、anchor、merge、自定义 tag，以及重复或大小写折叠冲突的 mapping key；完整嵌套 `install`、source metadata 与 artifact manifest 可通过验证。`config_sha256` 始终散列 Core 传入的 exact raw ConfigYAML bytes：

```yaml
worker_token_env: CPA_WRITER_WORKER_TOKEN
# sync_epoch: Writer 管理；通常不要手写
```

`worker_token_env` 必填。插件仅在 Configure/reconfigure 时解析环境变量并捕获非空 token。完整示例见 `config.example.yaml`。

## Management API

内部 Writer 路由同时依赖 CPA management middleware 与 `X-Sync-Config-Writer-Token`：

- `POST /v0/management/plugins/model-info/ingest` — 接收严格 JSON `{"catalog_base64":"..."}`，完整验证后原子替换缓存
- `GET /v0/management/plugins/model-info/writer-status` — 返回 `instance_id`、`reconfigure_seq`、`config_sha256`

只读缓存路由：

- `GET /v0/management/plugins/model-info/catalog`
- `GET /v0/management/plugins/model-info/last`
- `GET /v0/management/plugins/model-info/effective`

浏览器资源只在 Core 注册的 exact path `GET /v0/resource/plugins/model-info/index.html` 提供 HTML；其他 resource/API path 返回 404。

刷新由 UI 调用 Writer：

1. `POST /v0/management/plugins/sync-config-write/model-info/catalog`
2. 以返回的 `run_id` 轮询 `GET /v0/management/plugins/sync-config-write/status?run_id=...`
3. terminal 后重新读取 model-info 缓存视图

UI 不读取、存储或发送 worker token。它复用 Management Center 已保存的普通 management credential；无法读取时可临时手填，输入值只保留在当前页面内存。

## Catalog 规则

解码后最大 8,388,608 bytes。解析前的严格 token scan 同时限制结构复杂度，避免合法字节大小被大量微小节点放大：最多 4,096 个 models；任一 JSON object 最多 256 个成员；普通 array 最多 4,096 个元素；全 catalog 最多扫描 262,144 个 JSON values/members；object key 最多 256 bytes。字段上限按 UTF-8 byte length 计算：model `id`/`slug` 1,024，`display_name` 4,096，`visibility`、reasoning `effort`、input/output modality 128；reasoning levels 最多 32，input/output modalities 各最多 16。任何超限均返回 `catalog_invalid`/400，且不替换现有缓存。

`catalog_base64` 必须是 canonical padded standard base64（拒绝空白、URL-safe alphabet、缺失/错误 padding 与非零 pad bits）。成功 receipt 仅包含 `count` 和 decoded catalog exact bytes 的 lowercase SHA-256。模型可使用 `id`、`slug`，或同时使用 trim 后相同的两者；两者同时存在但不同会使整个 ingest 返回 `catalog_invalid` 并保留缓存。最大输入优先 `max_input_tokens`，否则 `max_context_window`；最大输出依次为 `max_tokens`、`max_output_tokens`、`max_completion_tokens`。成功空 catalog 保留为 `"models":[]`。

MIT
