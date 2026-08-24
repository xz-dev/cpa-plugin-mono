# auto-pull-models

CPA 插件：按 **openai-compatibility provider 名字** 自动（或手动）拉取上游 `/models`，用 **regex** 做只包含或排除，再写回该 provider 的 `models` 列表。这样 `/management.html#/ai-providers` 里的模型不用每次手点拉取。

插件 ID：`auto-pull-models`  
产物：`auto-pull-models.so`

## 能做什么

- 指定要同步哪些 provider（对应 CPA `openai-compatibility[].name`）
- 每个 provider 选一种规则：`include`（只保留匹配）或 `exclude`（丢掉匹配）
- `patterns` 是 Go RE2 正则，匹配上游模型 **id**（写入 `models[].name`）
- 已有 `alias` 默认保留；新模型 alias 默认等于 id
- 定时同步 + WebUI / Management API 立即同步
- 可按 provider + 上游模型 id 手工覆盖上下文、输入/输出上限和思维等级；手工值永远最后生效

不会改 API key、base-url、headers。默认只更新 `models` 的 name/alias。勾选 `upstream_meta` 或 `modelparams` 时，会顺带写入 `thinking.levels`。

## 配置

有两层：

1. CPA `config.yaml` 里启用插件，并指出 JSON 路径（可选，有默认值）
2. **真正的规则在 JSON 文件里**，可用 WebUI 改，也可以直接编辑文件

`config.yaml`：

```yaml
plugins:
  enabled: true
  dir: plugins
  configs:
    auto-pull-models:
      enabled: true
      config_file: plugins/auto-pull-models/config.json
```

JSON（默认 `plugins/auto-pull-models/config.json`）：

```json
{
  "interval": "1h",
  "management_base_url": "http://127.0.0.1:8317",
  "management_key_env": "",
  "management_key_file": "/CLIProxyAPI/mgmt.key",
  "keep_existing_aliases": true,
  "providers": {
    "openrouter": {
      "enabled": true,
      "mode": "include",
      "patterns": ["^openai/", "^anthropic/", "gpt-"]
    },
    "siliconflow": {
      "enabled": true,
      "mode": "exclude",
      "patterns": ["embed", "rerank", "-vl$"]
    },
    "ZCode": {
      "enabled": true,
      "mode": "exclude",
      "patterns": [],
      "codex_manifest": true,
      "upstream_meta": true,
      "overrides": {
        "glm-5.3": {
          "max_context_length": 262144,
          "max_input_tokens": 240000,
          "max_output_tokens": 22144,
          "thinking_levels": ["low", "high", "max"]
        }
      }
    }
  }
}
```

| 字段 | 含义 |
|---|---|
| `interval` | 自动同步周期，Go duration，如 `1h`、`30m`。空或 `0` 表示只手动同步 |
| `management_base_url` | CPA 管理 API 地址，容器内一般是 `http://127.0.0.1:8317` |
| `management_key_file` / `management_key_env` | 明文 management key。后台定时同步和 WebUI 都会用；WebUI 不再要求粘贴，优先读 Management Center，否则用服务端这个文件 |
| `keep_existing_aliases` | `true` 时已有 alias 不覆盖 |
| `modelparams_url` | 可选。默认 `https://modelparams.dev/api/v1/models.json` |
| `modelsdev_url` | 可选。默认 `https://models.dev/api.json` |
| `providers.<name>.enabled` | 是否同步这个 openai-compatibility provider |
| `providers.<name>.mode` | `include` 或 `exclude` |
| `providers.<name>.patterns` | 正则列表；`include` 且列表为空 = 一个都不留；`exclude` 且列表为空 = 全留 |
| `providers.<name>.codex_manifest` | `true` 时请求 `/models?client_version=1.0.0`，并按 Codex manifest 读取 `slug`、`supported_reasoning_levels`、`input_modalities`。仅对支持该协议的上游启用 |
| `providers.<name>.upstream_meta` | `true` 时从这次拉取的上游目录读取 reasoning 档位和模态；支持 OpenRouter 与 Codex manifest，不再额外请求 |
| `providers.<name>.modelparams` | `true` 时用 modelparams.dev Full catalog 填 `thinking.levels`。一次刷新只拉一次 catalog，所有勾选的渠道共用。若同时开了 `upstream_meta`，只补上游没给档位的模型 |
| `providers.<name>.modelsdev` | `true` 时用 models.dev 补齐上游目录缺失的上下文窗口 / 输出上限 / 模态。一次刷新只拉一次 catalog。上游已给出的值永远不会被覆盖 |
| `providers.<name>.overrides.<model>.max_context_length` | 精确匹配上游模型 id，最后覆盖 `max-context-length`；正整数生效，`0` 或省略表示不覆盖 |
| `providers.<name>.overrides.<model>.max_input_tokens` | 最后覆盖 `max-input-tokens` 目录元数据；正整数生效 |
| `providers.<name>.overrides.<model>.max_output_tokens` | 最后覆盖 `max-output-tokens` 目录元数据；正整数生效 |
| `providers.<name>.overrides.<model>.thinking_levels` | 精确匹配上游模型 id，最后覆盖 `thinking.levels`；支持 `none/minimal/low/medium/high/xhigh/max/ultra/auto` |

`providers` 里出现的名字必须已经在 CPA 的 AI Providers / `openai-compatibility` 里存在。插件用该 provider 的 `auth-index` 走管理 API `api-call` 去拉上游模型，不会把 API key 写进 JSON。

## 上下文窗口

勾选 `modelsdev` 的 provider，同步时会把每模型的 `max-context-length`
和 `max-output-tokens` 写入 config：上游目录声明的值优先，models.dev
只补缺口；两者都没有则留空。`overrides` 在上述来源之后应用，因此定时
同步不会覆盖手工值。`max-input-tokens` 没有可靠公共 catalog 来源，只能手工覆盖。

这些字段是目录元数据，不会截断请求。`max-input-tokens` / `max-output-tokens`
需要包含相应 OpenAI-compatible model 字段的 CPA 版本；旧版 CPA 会忽略它们。

## WebUI

插件加载后：

- 菜单：Management Center 里的 **Auto Pull Models**
- 或直接打开：`http://<host>:8317/v0/resource/plugins/auto-pull-models/index.html`

页面可以：

- 编辑并保存规则，包括每个 provider 的模型覆盖 JSON
- 立即同步（全部或单个 provider）
- 看上次结果（拉取数、写入数、错误）

保存 JSON 后 CPA 会 `plugin.reconfigure`，定时器按新 `interval` 重跑。

## Management API

均需 management key。

| 方法 | 路径 | 作用 |
|---|---|---|
| GET | `/v0/management/plugins/auto-pull-models/status` | 上次同步结果、当前配置摘要 |
| GET | `/v0/management/plugins/auto-pull-models/json` | 读取 JSON |
| PUT | `/v0/management/plugins/auto-pull-models/json` | 写入 JSON |
| POST | `/v0/management/plugins/auto-pull-models/sync` | 立即同步。Query `provider=<name>` 只同步一个 |

## 安装

```bash
make build
cp build/plugins/linux/amd64/auto-pull-models.so \
  /path/to/CPA/plugins/linux/amd64/auto-pull-models.so
```

容器内需要能读到明文 management key 才能定时跑，例如把宿主机 `~/CPA/.mgmt-key` 挂成 `/CLIProxyAPI/mgmt.key`，并在 JSON 里设置 `management_key_file`。

改 `.so` 后必须重启 CPA（或走插件安装接口卸载再加载）。

## 安全

进程内插件。本插件只访问 CPA 管理 API 和已配置 provider 的模型列表接口，不读 `auths/` 里的 OAuth 文件。JSON 里不要写 API key。
