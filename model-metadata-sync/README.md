# model-metadata-sync

Independent CPA plugin enriching metadata on existing exact upstream model names. It never adds, removes, renames, aliases, or reorders models.

Plugin ID/artifact: `model-metadata-sync` / `model-metadata-sync.so`.

## Ownership and precedence

Fields:

- `thinking.levels`
- `max-context-length`
- `max-input-tokens`
- `max-output-tokens`
- `input-modalities`
- `output-modalities`

Precedence preserves baseline OpenAI behavior while adding explicit native Claude input limits:

1. Existing model value starts preserved.
2. `upstream_meta` replaces thinking/modalities and fills missing context/output limits. Only the Claude adapter additionally maps explicit `max_input_tokens` into context + max input.
3. With a models.dev source, upstream context/output gets first fallback attempt even when full upstream metadata is off.
4. Ordered modelparams.dev sources may replace preserved thinking and concrete output limit.
5. Ordered models.dev sources fill unset fields only.
6. Positive manual overrides apply last.
7. Catalog errors stay visible and never change membership.

Core patch modes are `replace` for upstream/authoritative/manual values and `if-empty` for secondary completion. Only changed fields on exact names already present in sanitized descriptor are patched. Wire uses `operations[]`, each with exact `model` + per-field `{mode,value}`; no whole-model alias payload is sent.

## Selectors and adapters

- OpenAI-compatible: exact trimmed `name` + normalized `base_url`.
- Claude: current `config_index` + normalized effective `base_url` + normalized `prefix`.
- OpenAI profile: `openai_models`; optional Codex query `client_version=1.0.0`; every catalog page is fenced by current channel revision.
- Claude profile: `claude_models`; `limit=1000`, then `after_id=last_id` until `has_more=false`.

Claude maps explicit `max_input_tokens` to max input + context and `max_tokens` to max output. Thinking/capabilities copy only explicit fields. Modalities accept only `text` and `image`; no name inference, synthetic effort, document/PDF, or output modality.

## Production source examples

See [`config.example.json`](./config.example.json):

- 元流: `modelparams.dev/openai/subscription`, then `models.dev/openai`.
- Ollama Cloud: only `models.dev/ollama-cloud`; no manual params.
- ZCode: sidecar upstream metadata + Codex manifest only; no external sources or manual overrides.

## CPA plugin config

```yaml
plugins:
  enabled: true
  dir: plugins
  configs:
    model-metadata-sync:
      enabled: true
      config_file: plugins/model-metadata-sync/config.json
```

## Migration

Combined auto-pull provider-name config is rejected. Create explicit `channels[]` entries from current sanitized channel descriptors; copy filtering to auto-pull-models and metadata sources/overrides here. Preview both plugins paused. Never bind by provider name alone.

## Management routes

- `GET /v0/management/plugins/model-metadata-sync/status`
- `GET|PUT /v0/management/plugins/model-metadata-sync/json`
- `GET /v0/management/plugins/model-metadata-sync/channels`
- `GET /v0/management/plugins/model-metadata-sync/metadata-sources`
- `POST /v0/management/plugins/model-metadata-sync/preview`
- `POST /v0/management/plugins/model-metadata-sync/sync`

## Build

```bash
make test
make build
```

Install `build/plugins/linux/amd64/model-metadata-sync.so`, then restart or reload CPA plugin.
