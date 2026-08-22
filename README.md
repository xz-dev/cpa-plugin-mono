# cpa-plugin-mono

CLIProxyAPI (CPA) 插件单仓。每个子目录是一个独立插件，目录名等于插件 ID 和产物文件名。

| 插件 | 作用 |
|---|---|
| [`auto-pull-models`](./auto-pull-models) | 按 provider 定时/手动拉取上游模型列表，用 regex 做包含或排除，写回 `openai-compatibility` |

构建产物放到各插件的 `build/plugins/linux/amd64/<plugin-id>.so`，再复制到 CPA 的 `plugins/linux/amd64/`。
