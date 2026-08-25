# Split auto-pull-models acceptance contract

Baselines:

- `cpa-plugin-mono@24944ac7d06401b73343e62fe6147a9b47c64665`
- `CLIProxyAPI@8417d15ecaa3d5b1fabdb225c9e3865844aa7a84`

Actor: CPA operator.

Need: synchronize channel model membership independently from rich model metadata, without exposing channel credentials or allowing one writer to erase another writer's fields.

Value: scheduled catalog maintenance can cover OpenAI-compatible and native Claude channels safely while preserving existing source precedence and manual corrections.

## Scope

- `auto-pull-models` remains OpenAI-compatible membership/filter/alias synchronization.
- `model-metadata-sync` enriches existing models only, for explicitly selected OpenAI-compatible and Claude channels.
- `model-info` remains unchanged.
- CLIProxyAPI owns channel discovery, credential-bound catalog fetch, atomic channel mutation, persistence, and Claude rich-field propagation.
- No persistent channel UUID in this slice. Composite selectors fail closed on ambiguity or drift.
- No plugin direct-file mode.

## Acceptance examples

### AC-1 Sanitized channel inventory

Given CPA config contains OpenAI-compatible and Claude credentials, custom headers, proxy URLs, and API keys,
when an authenticated management client requests `GET /v0/management/model-channels`,
then response contains only allowlisted channel selector/state/model metadata fields,
and recursively contains no API key, token, cookie, configured header value, proxy URL, raw auth ID, or secret-derived identifier.

### AC-2 Channel-bound catalog authority

Given a selected live channel,
when plugin requests catalog through core channel-catalog operation,
then core derives catalog URL from current channel base and named profile, resolves current credential/proxy/custom headers internally, and returns only upstream status/body.

Caller cannot choose an arbitrary absolute URL or request headers. Cross-origin redirects, missing/stale credentials, unsupported query parameters, oversized bodies, and timeout fail without leaking headers or credentials.

For native Claude, direct `api.anthropic.com` API-key auth uses `x-api-key`; delegated/custom Claude upstreams use Bearer authentication; `anthropic-version: 2023-06-01` cannot be overwritten by inherited configured headers.

### AC-3 Selector drift fails closed

OpenAI-compatible selector is exact trimmed channel name plus shared normalized base URL. Zero matches returns not found; multiple matches returns conflict.

Claude selector is current `config_index` plus normalized effective base URL and normalized prefix. Reorder, base change, or prefix change returns conflict/not found rather than mutating another channel.

No operation treats name, array index, config index, or auth index alone as stable identity.

### AC-4 Membership preserves metadata

Given existing OpenAI-compatible models contain aliases and rich fields,
when `auto-pull-models` reconciles fetched exact upstream IDs,
then retained model nodes keep latest alias and all non-membership fields, missing upstream IDs are removed, new IDs are appended with default alias behavior, and no other channel changes.

A concurrent metadata change or stale revision causes conflict rather than lost update.

### AC-5 Metadata patches existing models only

Given an explicitly configured OpenAI-compatible or Claude channel,
when `model-metadata-sync` writes desired metadata,
then only these fields may change on exact existing upstream model names:

- `thinking.levels`
- `max-context-length`
- `max-input-tokens`
- `max-output-tokens`
- `input-modalities`
- `output-modalities`

`replace` writes authoritative/manual values. `if-empty` fills only empty fields. Absent means unchanged. Null/clear, missing model, ambiguous duplicate model name, unsupported modality, invalid field, and stale precondition reject the complete request with no partial write.

### AC-6 Existing metadata precedence remains identical

For each model and field, new plugin produces same desired value, source, status, reason, and attempted-source list as baseline combined implementation:

1. Existing configured value starts as preserved.
2. Enabled upstream metadata replaces thinking/modalities but only fills missing context/output limits.
3. When a `models.dev` source exists, upstream context/output are attempted before external fallback even if full upstream metadata is disabled.
4. Ordered `modelparams.dev` sources can authoritatively replace preserved thinking and concrete output limit.
5. Ordered `models.dev` sources fill only fields still unset.
6. Manual positive-value overrides apply last.
7. Source errors remain visible and do not alter model membership.
8. Repeat sync is unchanged/idempotent.

### AC-7 Production source rules migrate exactly

- 元流: `modelparams.dev/openai/subscription`, then `models.dev/openai`.
- Ollama Cloud: only `models.dev/ollama-cloud`; no manual parameters.
- ZCode: only sidecar upstream metadata; no stale external sources or manual overrides from example config.
- `codex_manifest` query behavior remains with catalog discovery; Codex metadata decoding moves to metadata plugin.

Migration fails on ambiguous composite selectors; it never binds rules by provider name alone.

### AC-8 Native Claude metadata propagation

Given Claude `/v1/models` explicitly returns supported values,
when metadata sync runs,
then adapter paginates with `limit`, `after_id`, and `last_id` until `has_more=false`; maps `max_input_tokens` to max input and context, `max_tokens` to max output, and copies only explicit thinking/capability data.

Only `text` and `image` modalities are accepted. No model-name inference, synthetic `auto`/`none`, document/PDF modality, or output modality occurs.

Persisted values survive management save/reload/restart, affect Claude model hashes, reach registry `ModelInfo`, and appear on intended client model catalog path.

### AC-9 Targeted persistence

Given model nodes contain comments, aliases, ordering, style, and unknown keys,
when membership or metadata mutation succeeds,
then only selected channel `models` YAML node changes, file inode remains unchanged, complete YAML is validated before write, write/truncate/fsync succeeds, in-memory config and reload snapshot publish only after persistence, and ordinary write failure restores original bytes and leaves in-memory state unchanged.

Same-inode persistence is process-atomic but not crash-atomic; rollback backup and recovery procedure document crash window.

### AC-10 Split plugin independence

Given both plugins are enabled,
when membership and metadata schedules overlap,
then core atomic operations prevent stale whole-slice replacement. Correctness does not depend on ticker staggering or independent plugin locks.

Newly discovered models may remain unenriched until next metadata cycle; status reports expose this bounded lag.

### AC-11 Safe rollout and rollback

Core support deploys dormant first. Both split plugin artifacts/configs install paused. Migration and preview complete before legacy metadata ownership is disabled. One explicit channel canaries through write, restart, registry/catalog validation, then one scheduled cycle before expansion.

Rollback disables metadata writer first, pauses membership writer, restores pre-migration CPA YAML and plugin configs, validates reload/catalogs, then downgrades core. No interval has two metadata writers.

## Required evidence

Focused tests:

- Descriptor secret absence and selector canonicalization/drift.
- Catalog origin/auth/header precedence/redirect/body cap/timeout/Claude pagination.
- Atomic membership and metadata conflict behavior.
- YAML comments/unknown fields/inode/failure rollback.
- Claude schema/hash/reload/registry/catalog propagation.
- Golden parity fixtures for baseline metadata values and provenance.
- Black-box management E2E with mock OpenAI-compatible and native Claude channels.
- Production canary and rollback receipts.

User acceptance remains separate from automated evidence.
