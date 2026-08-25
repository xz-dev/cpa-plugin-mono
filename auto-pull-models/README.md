# auto-pull-models

CPA membership-only plugin for OpenAI-compatible channels.

Plugin ID/artifact: `auto-pull-models` / `auto-pull-models.so`.

## Ownership

- Select channel by exact trimmed `name` + normalized `base_url`.
- Fetch catalog through core `model-channels/catalog` profile `openai_models`, fenced by current channel revision.
- Optional Codex catalog query `client_version=1.0.0`.
- Apply include/exclude Go RE2 filters.
- Reconcile exact upstream model names through atomic core membership operation.
- Preserve existing aliases when configured.
- Expose status, JSON, channels, preview, sync, WebUI, independent timer.

No metadata enrichment, external metadata sources, direct config-file writes, channel credentials, caller-selected upstream URLs, or caller-supplied headers.

## CPA plugin config

```yaml
plugins:
  enabled: true
  dir: plugins
  configs:
    auto-pull-models:
      enabled: true
      config_file: plugins/auto-pull-models/config.json
```

See [`config.example.json`](./config.example.json). Important fields:

- `interval`: Go duration; `0` means manual only.
- `management_base_url`, `management_key_file`, `management_key_env`: management access.
- `keep_existing_aliases`: core preserves aliases for retained upstream names.
- `channels[].selector`: exact `{name, base_url}` composite selector. Name-only binding is rejected.
- `channels[].mode`: `include` or `exclude`.
- `channels[].patterns`: regex list. Empty include keeps none; empty exclude keeps all.
- `channels[].codex_manifest`: adds `client_version=1.0.0` to core-owned catalog fetch.

## Combined-config migration

Old `providers` map, `upstream_meta`, `metadata_sources`, overrides, `modelparams_url`, `modelsdev_url`, `write_mode`, and `config_path` are rejected with a clear schema error. Migration:

1. Replace provider-name map with `channels[]` and copy exact current channel `name` + normalized `base_url`.
2. Keep filtering and `codex_manifest` here.
3. Move metadata settings into independent `model-metadata-sync` config.
4. Preview both plugins before enabling timers. Ambiguous selectors fail closed.

## Management routes

- `GET /v0/management/plugins/auto-pull-models/status`
- `GET|PUT /v0/management/plugins/auto-pull-models/json`
- `GET /v0/management/plugins/auto-pull-models/channels`
- `POST /v0/management/plugins/auto-pull-models/preview`
- `POST /v0/management/plugins/auto-pull-models/sync`

`channel=<selector-key>` scopes preview/sync; legacy `provider=<name>` remains only as UI query convenience and still resolves against configured composite selector.

## Build

```bash
make test
make build
```

Install `build/plugins/linux/amd64/auto-pull-models.so`, then restart or reload CPA plugin.
