# auto-pull-models

CLIProxyAPI native plugin that plans OpenAI-compatible model membership changes. It is a pure worker for `sync-config-write`: it never reads or writes CPA files, calls CPA management APIs, or performs provider HTTP.

## Configuration

Configuration comes only from `plugins.configs.auto-pull-models`:

```yaml
worker_token_env: CPA_WRITER_WORKER_TOKEN
sync_epoch: initial
channels:
  - enabled: true
    selector:
      name: OpenRouter
      base_url: https://openrouter.ai/api/v1
    mode: exclude
    patterns:
      - '^free/'
```

`worker_token_env` must resolve to the same non-empty process environment value configured for Writer. Selectors use exact trimmed channel names and canonical HTTPS base URLs. `mode` is `include` or `exclude`; patterns are Go regular expressions. One Writer run processes every enabled configured selector in order.

Legacy `config_file`, intervals, management credentials, `keep_existing_aliases`, `codex_manifest`, JSON/UI routes, and direct sync/preview routes are intentionally unsupported.

## Private Writer routes

- `POST /v0/management/plugins/auto-pull-models/plan`
- `GET /v0/management/plugins/auto-pull-models/writer-status`

Both require `X-Sync-Config-Writer-Token`. Users trigger Writer's `/run/auto-pull-models` route instead of calling these routes.

Planner requests use exact case-sensitive JSON with base64-framed full config bytes. Each continuation is bound to one exact snapshot, configuration generation, and planning attempt, and each fetch receipt is accepted once; a new attempt or reconfigure invalidates the prior continuation. Provider credentials are resolved only through stock `host.auth.list`, `host.auth.get_runtime`, and `host.auth.get`; only one active file-backed identity matching the selected channel is accepted. Planner returns a secret-free `$TOKEN$` fetch descriptor. Writer performs stock management `/api-call`, then returns only bounded provider body bytes for mandatory post-fetch identity revalidation.

Retained models preserve complete YAML mappings, aliases, metadata, order-relative comments, and unknown fields. New models contain only `name`; excluded models are removed; upstream order wins. Planner never enriches metadata or mutates `plugins.*`.

## Build and test

```bash
env -u GOROOT go test ./...
env -u GOROOT go test -race ./...
make build-local
```
