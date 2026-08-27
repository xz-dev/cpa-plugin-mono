# model-metadata-sync

Pure metadata planner for the four-plugin CLIProxyAPI architecture.

Plugin ID/artifact: `model-metadata-sync` / `model-metadata-sync.so`.

## Authority

`model-metadata-sync` proposes changes to existing exact-name models. It never persists configuration and never adds, removes, renames, aliases, or reorders models.

Owned CPA YAML keys:

- `thinking` (the planner updates `levels` while retaining other thinking keys)
- `max-context-length`
- `max-input-tokens`
- `max-output-tokens`
- `input-modalities`
- `output-modalities`

`display-name`, alias, membership, order, plugin settings, credentials, and every unrelated node remain unchanged. `sync-config-write` independently validates that boundary before any PUT.

## Runtime contract

The plugin receives settings only through native `ConfigYAML` from `plugins.configs.model-metadata-sync`. See [`config.example.yaml`](./config.example.yaml).

It exposes only:

- `POST /v0/management/plugins/model-metadata-sync/plan`
- `GET /v0/management/plugins/model-metadata-sync/writer-status`

Both require normal CPA management authentication plus `X-Sync-Config-Writer-Token`. The plugin resolves that token from the environment-variable name in `worker_token_env`; the token value is never stored in YAML or returned by status.

A planner run processes every configured selector. Writer supplies a base64-framed exact full-config snapshot and lowercase SHA-256 version. The planner returns either a bounded secret-free fetch descriptor or a complete base64-framed proposal. Continuations bind the exact snapshot, version, ConfigYAML hash, reconfigure generation, random attempt, request, page, and prior results. One pending receipt is consumed once; replay, cross-attempt substitution, reconfigure, token rotation, and shutdown fail closed.

The plugin has no config/key-file access, JSON persistence, UI, ticker, direct Core/Writer management client, or outbound HTTP client.

## Provider catalogs

Credential-bound OpenAI-compatible and Claude catalogs require exactly one active file-backed CPA auth exposed consistently by stock `host.auth.list`, `host.auth.get_runtime`, and `host.auth.get`. Config-only credentials fail closed.

The planner emits only validated `auth_index` and `$TOKEN$` headers. Writer relays the request through stock `/api-call`; Core alone substitutes the credential and applies its proxy policy. The planner revalidates the same auth after each fetch before using the body or producing another descriptor/proposal.

Catalog forms:

- OpenAI-compatible: exact normalized selector and `/models`.
- Optional `codex_manifest`: exact `/models?client_version=1.0.0`; Writer permits this query only for metadata-sync.
- Claude: `/v1/models?limit=1000`, then strict `after_id` progression, at most 100 pages.
- Public metadata: fixed `https://modelparams.dev/api/v1/models.json` and `https://models.dev/api.json`, without credentials.

Every page and cumulative body are bounded to 8 MiB. Malformed, duplicate, folded-key, ambiguous-ID, cursor, auth, or continuation data fails closed without using partial data or leaking response content.

## Precedence

For each existing model:

1. Existing values begin preserved.
2. `upstream_meta` replaces explicit thinking/modalities and fills missing context/output limits. Claude additionally maps explicit `max_input_tokens` to context and max input.
3. When a `models.dev` source is configured, explicit upstream context/output receives the first fallback attempt even if full upstream metadata is off.
4. Ordered `modelparams.dev/<provider>/<authType>` sources may replace preserved thinking and concrete output limit.
5. Ordered `models.dev/<provider>` sources fill fields still missing.
6. Positive per-model overrides apply last.

OpenAI upstream `max_input_tokens` remains ignored for legacy parity; configure an existing value or explicit override. Modalities are limited to `text` and `image`. Source identity and provenance are per field and never inferred from similar model names.

Retained `thinking` mappings keep budget fields, comments, and order while `levels` changes.

## Configuration

```yaml
plugins:
  enabled: true
  dir: plugins
  configs:
    model-metadata-sync:
      worker_token_env: CPA_WRITER_WORKER_TOKEN
      channels:
        - enabled: true
          kind: openai-compatibility
          selector:
            name: Example
            base_url: https://provider.example/v1
          upstream_meta: true
          metadata_sources:
            - modelparams.dev/openai/subscription
            - models.dev/openai
```

OpenAI selectors are exact trimmed `name` plus normalized HTTPS `base_url`. Claude selectors use `config_index`, normalized effective HTTPS `base_url`, and normalized `prefix`. Duplicate selectors, unsafe URLs, unknown fields, legacy file/management/timer fields, plaintext tokens, and ambiguous source order are rejected.

Writer injects `sync_epoch`; operators do not manage it manually. Environment changes take effect only through reconfigure/restart.

## Build and verification

```bash
make test
make build
```

Direct Go checks:

```bash
go test ./...
go test -race ./...
go vet ./...
go mod verify
```

Install the reviewed c-shared artifact only with the matching reviewed Writer and fixed Core composition. Timers remain disabled until four-plugin integration and rollback acceptance pass.
