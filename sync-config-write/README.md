# sync-config-write

Native CPA plugin for serialized configuration orchestration.

Implemented slices:

- direct `plugins.configs.sync-config-write` ConfigYAML validation and environment-only secret indirection;
- fixed no-proxy numeric-loopback CPA client;
- persistent process instance, global queue/worker/status registry, operation coalescing, startup block, bounded retention, deadline-preserving reconfigure, and phase/attempt reporting;
- exact-byte snapshot versioning and base64 planner envelopes;
- path-aware membership and metadata ownership validation;
- serialized full-config PUT, exact reread verification, four-plugin `sync_epoch` injection, runtime convergence evidence, and fail-closed reconciliation;
- bounded planner continuation relay through stock management `/api-call`;
- Writer-only fixed model-info catalog refresh and token-protected ingest;
- five Writer management routes.

Provider relay accepts only strict JSON envelopes with exact case-sensitive field names, GET/HTTPS descriptors, point-in-time callback-validated `auth_index` plus literal credential placeholders, and these narrow catalog targets. Final planner envelopes require exactly `base_version`, complete `config_base64`, and `report.changed`; intermediate envelopes require non-null `next_fetch` and forbid config/report fields. Selected provider entries and header mappings must be direct YAML mappings; aliases and merge keys fail closed. Runs have no selector-scope field and process all selectors configured in the target planner.

- OpenAI-compatible: exact selected snapshot base URL plus `/models`;
- Claude: selected snapshot origin plus `/v1/models?limit=1000` and optional nonempty `after_id`;
- public metadata: exact `https://modelparams.dev/api/v1/models.json` or `https://models.dev/api.json`.

OpenAI catalogs require canonical `Authorization: Bearer $TOKEN$`; official Anthropic requires canonical `x-api-key: $TOKEN$` plus fixed `anthropic-version: 2023-06-01`; compatible Claude defaults to Authorization unless its selected snapshot entry explicitly configures the x-api-key contract. Other catalog-safe headers must exactly match the selected snapshot entry; the only unconfigured literal allowed is optional `Accept: application/json`. Continuations and cumulative provider bodies are limited to 8 MiB, with at most 100 pages; repeated continuations, request IDs, descriptors, or Claude cursors fail closed. Writer sends only exact body base64 back to the same planner.

Callback validation and Core credential lookup are not atomic. If a file-backed auth disappears after planner preflight, stock Core may issue one request containing literal `$TOKEN$`; every successful fetch still returns through the planner for mandatory list/runtime/get revalidation, and persistent drift discards the body with no proposal or PUT. Exact remove/restore ABA remains unprovable without Core hardening. Current Writer tests model this boundary; exact Core race execution remains an integration gate.

Model-info refresh selects exactly one trimmed root `api-keys` scalar by configured lowercase SHA-256, performs only direct no-proxy `GET /v1/models?client_version=1.0.0`, and caps raw body at 8 MiB. Before ingest it re-GETs config and rejects version drift without replay; it then sends only `catalog_base64` to private `/model-info/ingest`. Success requires the exact ingest receipt count and catalog SHA-256. It never performs config PUT and remains available while config writes are blocked.

Deferred by design: worker-plugin conversion and UI. Until worker routes are converted, startup reconciliation safely remains blocked and write runs cannot complete.

## Build

```sh
env -u GOROOT go test ./... -count=1
go test -race ./... -count=1
go vet ./...
make build-local
```
