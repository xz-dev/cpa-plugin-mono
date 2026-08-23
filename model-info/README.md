# model-info

CPA 插件:在 Management Center 里查看所有模型的元数据表——上下文窗口、
输出上限(max_tokens)、推理等级、输入/输出模态。

数据来自本机 CPA 的 Codex client 目录
(`GET /v1/models?client_version=1.0.0`),经管理 `api-call` 代理获取,
浏览器不接触任何 API key。

## 使用

安装 `.so` 后打开 Management Center 的 **Model Info** 菜单,或直接访问
`http://<host>:8317/v0/resource/plugins/model-info/index.html`。
支持搜索、按 provider 过滤、点列头排序;上下文/输出上限缺失的单元格
会标红,方便发现 auto-pull-models 还没补齐的模型。

## 配置

插件 YAML(可选):

```yaml
config_file: plugins/model-info/config.json
```

JSON 字段(全部可选):

| 字段 | 默认 | 说明 |
|---|---|---|
| `management_base_url` | `http://127.0.0.1:8317` | CPA 管理 API 地址 |
| `management_key_env` | 空 | management key 环境变量名 |
| `management_key_file` | `/CLIProxyAPI/mgmt.key` 建议同 auto-pull-models | management key 文件 |

## Management API

- `GET /v0/management/plugins/model-info/catalog` — 立即拉取并返回目录
- `GET /v0/management/plugins/model-info/last` — 上次结果(缓存)

MIT
