# Four-plugin standard-CPA architecture contract

Status: approved implementation and deployment contract.

This document is the durable source of truth for the four-plugin design. It records stable decisions, wire contracts, safety invariants, verification gates, and residual boundaries. Session progress, branch choreography, temporary paths, and review transcripts do not belong here.

## 1. Fixed scope

### Core composition

Build CLIProxyAPI from exactly:

1. release `v7.2.142` at `1f53b2eb`;
2. PR #5246 commits `908c46d2`, `e469ef03`;
3. PR #5247 commit `eea510a8`;
4. PR #5264 commits `fd70b12a`, `b2b2d61d`.

Apply the five commits in that order. Exclude `07cb171d`, model-channel/revision/CAS APIs, ancestry skips, fixups, newer bases, and unrelated Core changes. Before each build:

- verify tag and commit objects;
- require every cherry-pick to apply without conflict, adaptation, or empty result;
- record source SHA to resulting SHA and verify stable patch-id equivalence;
- require the final diff from `1f53b2eb` to contain exactly those five patches.

Any base or patch-set change requires a new source review and contract revision.

### Supported deployment shape

- One CPA process owns one config file.
- One unchanged live `sync-config-write` instance coordinates every cooperating plugin write.
- All four plugins use CPA `plugins.configs.<id>` and native register/reconfigure `ConfigYAML`.
- Provider identities selected for credential-bound catalogs must resolve to exactly one active file-backed CPA auth through stock `host.auth.list`, `host.auth.get_runtime`, and `host.auth.get`.
- Config-only API-key auths are unsupported by those callbacks in this Core composition and fail closed.

### Explicit residual boundaries

Stock CPA has unconditional full-config PUT, not CAS. Its GET does not share the PUT handler lock, and PUT validates before locking. Therefore Writer serializes only cooperating calls routed through its live instance.

Outside the guarantee:

- direct stock config PUT/PATCH overlapping a Writer commit;
- another process or Writer instance sharing the same config;
- crash-atomic persistence: Core uses truncate/write/file-fsync rather than atomic rename plus directory fsync;
- remove/restore ABA of provider auth;
- provider auth removal between callback preflight and stock `/api-call` lookup;
- Core buffering request/provider bodies before plugin-side size validation;
- stock `/api-call` timeout and redirect behavior.

Operators pause timers and avoid direct config mutations during manual changes. Observable post-PUT drift is detected, but detection is not transaction isolation.

## 2. Authority map

| Component | Owns | Must not own |
|---|---|---|
| `sync-config-write` | Management credential, authoritative GET/PUT, serialized orchestration, provider-fetch relay, structural ownership validation, convergence/reconcile, model-info catalog fetch | Provider membership policy, metadata precedence, model-info rendering |
| `auto-pull-models` | Membership proposals for configured OpenAI-compatible selectors | Metadata, persistence, management calls, outbound provider HTTP |
| `model-metadata-sync` | Six metadata fields on existing exact-name models | Membership/order/name/alias, persistence, management calls, outbound provider HTTP |
| `model-info` | Read-only catalog cache/effective views and validated ingest | Config snapshots, management/proxy credentials, config writes, outbound Core/provider fetches |

Writer is the sole orchestrator and management-credential holder. Planners are pure per-request transformations plus stock host-auth identity checks. `model-info` receives only a bounded catalog payload.

## 3. Trust and secret boundaries

### Management and worker authorization

Writer resolves the plaintext CPA management credential from the environment-variable name configured as `management_key_env`. No other plugin configures, resolves, returns, logs, or retains it.

Internal worker routes require both:

1. normal CPA management middleware; and
2. `X-Sync-Config-Writer-Token`, resolved from the common environment-variable name configured as `worker_token_env`.

Workers ignore the transient management `Authorization` header included by stock dispatch. Status and error responses never expose it.

### Provider credentials

Credential-bound planners use only `host.auth.list/get_runtime/get` during an active plan. They require one matching active file-backed identity, discard physical auth JSON after identity validation, and emit only validated `auth_index` plus a literal `$TOKEN$` header template.

Writer sends that descriptor to stock `POST /v0/management/api-call`. Core substitutes or refreshes the token and applies proxy priority. Writer and planners never receive the provider token.

Preflight and `/api-call` lookup are not atomic. If auth disappears between them, Core can send literal `$TOKEN$` to the already allowlisted provider. Mandatory post-fetch callback revalidation discards the body and prevents proposal/PUT while drift remains observable. This design claims no zero-network-attempt guarantee and cannot exclude exact remove/restore ABA.

### Full snapshot trust

Planner snapshots are intentionally complete and unredacted. The two planners are trusted native plugins and may see root API keys and provider/config secrets. They use snapshot secrets only where this contract explicitly permits identity/catalog derivation.

The four implementations must not:

- read, write, rename, watch, or lock CPA/plugin config or key files;
- maintain external plugin JSON or key files;
- log snapshots, proposal bytes, headers, provider bodies, physical auth JSON, or parser excerpts containing source text;
- include secrets in status, metrics, persistent state, panic text, URLs, queries, or error envelopes;
- use `host.http.do` for credential-bound provider requests or secret-bearing loopback calls.

These are cooperative controls among reviewed native plugins, not sandboxing against a malicious plugin.

## 4. Routes and status

### Writer routes

- `POST /v0/management/plugins/sync-config-write/run/auto-pull-models`
- `POST /v0/management/plugins/sync-config-write/run/model-metadata-sync`
- `POST /v0/management/plugins/sync-config-write/model-info/catalog`
- `POST /v0/management/plugins/sync-config-write/reconcile`
- `GET /v0/management/plugins/sync-config-write/status`

### Worker routes

- `POST /v0/management/plugins/auto-pull-models/plan`
- `POST /v0/management/plugins/model-metadata-sync/plan`
- `POST /v0/management/plugins/model-info/ingest`
- `GET /v0/management/plugins/<worker>/writer-status`

Planner `writer-status` returns exactly `instance_id`, `reconfigure_seq`, `config_sha256`, and `active_plan`. Model-info status omits `active_plan`. No status returns config, token, credential, attempt, request, or catalog payload.

### Asynchronous run model

Run/catalog/reconcile triggers enqueue work and normally return `202` with a 128-bit `crypto/rand` base64url `run_id`. Writes blocked by unresolved state return sanitized `409 writer_blocked`; reconcile remains callable.

One Writer worker goroutine executes jobs globally. Required behavior:

- at most one queued/running job per operation; duplicate ticks coalesce and manual overlap returns the existing run;
- all config writes serialize; read-only model-info/reconcile work may pass writes retained behind a blocker;
- status states are `queued|planning|fetching|committing|waiting_reconfigure|reconciling|succeeded|failed|uncertain|blocked`;
- retain at most 32 ordinary completed statuses plus current blocker/reconcile state, in memory only;
- status includes only operation, state, attempt count, timestamps, versions, changed flag, sanitized error code, blocking run ID, and Writer identity tuple;
- each active job uses an immutable settings snapshot.

Periodic operations use absolute next-run deadlines. A `sync_epoch`-only reconfigure preserves deadlines; changing one interval resets only that operation from configure time; disabling removes only that deadline. Expired writes remain due while blocked and enqueue exactly once after reconciliation. Restart creates fresh deadlines after startup reconciliation; no persisted catch-up exists.

## 5. Snapshot and planner protocol

### Snapshot envelope

Writer obtains exact bytes from stock `GET /v0/management/config.yaml` and sends:

```json
{
  "version": "<64 lowercase hex digits>",
  "config_base64": "<base64 of exact raw GET bytes>"
}
```

`version` is lowercase SHA-256 of exact decoded GET bytes. No newline, YAML, comment, or line-ending normalization occurs before hashing. Base64 prevents Core management JSON sanitization from changing payload bytes. Snapshot and version live only for the current attempt.

### Planner result union

Final proposal:

```json
{
  "base_version": "<exact request version>",
  "config_base64": "<base64 of complete proposed YAML>",
  "report": {"changed": true}
}
```

Intermediate fetch request:

```json
{
  "base_version": "<exact request version>",
  "next_fetch": {
    "request_id": "<opaque step id>",
    "kind": "<openai_models|claude_models|modelparams|modelsdev>",
    "selector": {"channel_name": "<name>", "base_url": "<normalized URL>"},
    "auth_index": "<validated index or empty for public metadata>",
    "method": "GET",
    "url": "https://<trusted origin>/<fixed path>",
    "header": {"Authorization": "Bearer $TOKEN$"},
    "continuation_base64": "<base64 of secret-free state>"
  }
}
```

An envelope contains exactly one of `config_base64` or `next_fetch`. Field names are exact and case-sensitive; duplicate or case-fold-equivalent fields fail closed. `report` is diagnostic only and never grants commit authority.

### Continuation receipt

Writer repeats original `version` and `config_base64`, then adds:

```json
{
  "continuation_base64": "<exact planner state>",
  "fetch_result": {
    "request_id": "<exact requested id>",
    "status_code": 200,
    "body_base64": "<base64 of exact valid-UTF-8 body>"
  }
}
```

Planner continuation state binds exact snapshot hash, request version, ConfigYAML hash, reconfigure generation, random attempt ID, channel/page index, auth index, request ID, and accumulated prior results.

Each planner service maintains at most one live attempt and one pending request:

- a new valid initial request atomically replaces any older attempt;
- each pending request ID is consumed once before catalog processing;
- concurrent/exact replay has one winner;
- stale, cross-attempt, cross-generation, snapshot/version/request substitution fails closed;
- successful reconfigure, token rotation through reconfigure, shutdown, completion, or matching-attempt failure invalidates pending state;
- a late old attempt cannot clear or register against a newer attempt;
- multi-page/channel flow registers the next one-time request under the same attempt.

Lost responses are not replayable. Writer follows timeout/block/reconcile behavior and starts any later run from a new snapshot.

### Descriptor validation and relay

Writer independently validates every descriptor:

- known `kind`; `GET`; HTTPS; no userinfo or fragment;
- fixed compiled origin/path/query derived from the selected snapshot entry;
- exact selector identity and safe header allowlist;
- literal `$TOKEN$`, never a credential value;
- selected OpenAI/Claude mappings are direct mappings without mutable-path aliases or merge keys;
- public metadata descriptors have empty auth identity;
- decoded continuation, each page, and cumulative provider bytes are each bounded to 8 MiB; Claude additionally has a 100-page bound and strict cursor progress.

Writer invokes stock `/api-call` through its fixed direct loopback client and omits `proxy_url`. It accepts only 2xx, discards response headers, and forwards only bounded exact body bytes. The planner repeats host-auth identity validation before using a body, requesting another page, or returning a proposal.

Official Anthropic uses `x-api-key: $TOKEN$` and fixed `anthropic-version: 2023-06-01`; compatible gateways use the verified host-specific form. Claude pagination is `limit=1000`, `after_id=last_id`, `has_more`, with strict progress.

Public `modelparams.dev` and `models.dev` fetches use fixed compiled HTTPS origins and paths without `auth_index` or credential headers.

## 6. Writer plan and commit state machine

### Plan/fetch loop

1. Require startup/block reconciliation clear.
2. GET authoritative config and compute `base_version`.
3. Invoke one planner with the private worker token. A run processes every selector configured in that planner; trigger routes have no selector scope.
4. Validate and relay each `next_fetch`, then call the planner continuation with the original snapshot.
5. Validate the final proposal and operation-specific ownership.
6. Capture pre-PUT Writer and worker identity tuples.
7. Enter the serialized commit algorithm.
8. On pre-PUT version conflict only, discard proposal, continuation, and pages; restart from step 2 up to `max_version_retries` additional attempts.
9. Never rebase a serialized proposal or automatically replay an uncertain/interrupted PUT.

The commit mutex is not held during planning/provider fetch.

### Serialized commit algorithm

1. Validate envelope, exact echoed version, base64, YAML, and protocol bounds.
2. Acquire the process-local mutex whose identity survives ordinary Writer reconfigure.
3. GET current raw config and hash exact bytes.
4. Return typed `version_conflict` if hash differs from `base_version`.
5. Skip PUT when proposal bytes are identical or the structural validator finds zero owned-node changes.
6. For a real change, generate one fresh 128-bit `sync_epoch` and inject it into the four existing plugin ConfigYAML subtrees.
7. Predict `expectedPersisted` using an exact plugin reproduction of Core `NormalizeCommentIndentation`.
8. PUT through stock full-config management API.
9. Re-GET authoritative config and require exact byte equality with `expectedPersisted`.
10. Require runtime reconfigure convergence described below.
11. Record authoritative version/outcome before releasing the mutex.

Any post-PUT transport failure, byte mismatch, or convergence failure is terminal `uncertain`, blocks later writes, and is never replayed.

### Structural ownership validation

Parse base/proposal as single-document `yaml.Node` trees. Reject duplicate mapping keys and ambiguous aliases/merge keys in mutable paths. Compare every non-owned node path-aware by kind, tag, value, style, anchor/alias target, comments, and sequence/mapping order; ignore only parser positions.

Membership proposal may change zero or more selected OpenAI-compatible `models` sequences in one no-scope run. Validate each changed provider independently:

- retained exact-name model mappings are node-identical, including alias and metadata;
- removals and desired-order reordering are allowed;
- new entries are exactly `{name: <unique non-empty name>}`;
- no metadata or unrelated provider/root/plugin node changes.

Metadata proposal may change only these keys on existing exact-name models:

- `thinking`
- `max-context-length`
- `max-input-tokens`
- `max-output-tokens`
- `input-modalities`
- `output-modalities`

Membership, sequence order, name, alias, and all other nodes remain identical. `display-name` is not owned.

Writer injects `sync_epoch` only after proposal validation. Plugin topology, artifact/store/path/version/release identity, root API keys, remote-management fields, and unrelated channels are immutable during model runs.

### Runtime convergence

Before PUT, capture Writer local status and all three worker status tuples. After PUT, every unchanged process instance must report:

- greater `reconfigure_seq`; and
- exact SHA-256 of injected ConfigYAML containing the fresh common epoch.

Writer reproduces Core `runtimeConfigYAML` normalization and tests it against the fixed Core composition. Each status request has the fixed loopback deadline. Failure records `persisted_runtime_uncertain` and blocks writes.

### Reconciliation

Writer starts at `startup_reconcile_required`. Only `POST .../reconcile` can clear a blocker, and its `202` acknowledgement alone never clears anything.

- `planner_stalled`: both planner statuses must come from live instances with `active_plan:false`.
- startup or persisted-runtime uncertainty: re-GET authoritative config, derive exact expected hashes for all four plugin subtrees, require matching live instance/hash tuples and no active planner.
- when pre-PUT tuples survive, require sequence advancement; after process restart, exact hashes plus fresh live instance IDs are sufficient.

Record authoritative version and evidence timestamps, then clear only the same blocker under the mutex. Concurrent blocker/version change fails reconciliation. Reconcile never PUTs.

## 7. Domain transformations

### Selectors

- OpenAI-compatible: trimmed channel `name` plus normalized `base-url`; duplicates fail.
- Claude: configured stable `config_index`, then verify normalized effective `base-url` and `prefix` before every modification/fetch step.

### Membership

For each desired upstream name:

- retain the entire existing mapping when present;
- create only `{name: <upstream-name>}` when new;
- remove excluded names;
- emit upstream desired order;
- reject duplicate existing names.

Retained aliases always survive. New models omit alias. `keep_existing_aliases` does not exist.

### Metadata

Operate only on exact existing names. Preserve membership, order, name, alias, unrelated keys, and comments. Apply configured source precedence and per-field `replace`/`if-empty` decisions only to the six owned keys. Recompute all decisions from a fresh snapshot after a version conflict.

## 8. Model-info catalog contract

Writer alone fetches `GET /v1/models?client_version=1.0.0` through a dedicated direct-loopback client. It selects exactly one existing root `api-keys` scalar by configured lowercase SHA-256 fingerprint after `strings.TrimSpace`, sends that normalized key as bearer auth, then immediately re-GETs authoritative config.

If config version changed, discard the catalog and record terminal `version_conflict`; do not replay automatically. Otherwise send model-info exactly:

```json
{"catalog_base64":"<base64 of exact response bytes>"}
```

Limit is 8,388,608 decoded bytes:

- Writer streams at most limit + 1 and skips ingest on overflow.
- Core necessarily buffers the encoded management request before plugin validation.
- Model-info independently decodes, bounds, strictly parses, and atomically replaces cache only after success.
- Failure preserves prior cache.
- Success returns only `count` and lowercase decoded-byte `catalog_sha256`.
- Model-info never receives full config or proxy key and ignores transient management Authorization.

Its existing catalog/effective routes remain read-only. UI refresh triggers Writer, polls Writer status, then reloads those views.

## 9. Loopback client

Writer uses a dedicated stdlib client only for stock CPA and worker routes:

- origin is numeric `http://127.0.0.1:<port>` or `http://[::1]:<port>`;
- reject HTTPS, non-loopback host, userinfo, origin path/query/fragment, and caller-selected destination;
- clone default transport with `Proxy=nil`;
- disable redirects;
- construct fixed paths in code;
- enforce a 120-second request deadline.

Planner/provider transport timeout handling:

- before PUT, timed-out planner with cleared `active_plan` records `loopback_timeout`;
- true/unavailable `active_plan` records `planner_stalled` and blocks writes;
- after PUT, any timeout records `persisted_runtime_uncertain`.

A late pure planner result has no persistence capability. It may leave one bounded pending request until the next attempt/reconfigure/shutdown, but cannot commit or supersede newer attempt state.

## 10. Configuration

```yaml
plugins:
  configs:
    sync-config-write:
      core_origin: http://127.0.0.1:8317
      management_key_env: MANAGEMENT_PASSWORD
      model_info_proxy_api_key_sha256: "<64 lowercase hex digits>"
      worker_token_env: CPA_WRITER_WORKER_TOKEN
      auto_pull_interval: 0s
      metadata_sync_interval: 0s
      model_info_interval: 0s
      max_version_retries: 2
    auto-pull-models:
      worker_token_env: CPA_WRITER_WORKER_TOKEN
    model-metadata-sync:
      worker_token_env: CPA_WRITER_WORKER_TOKEN
    model-info:
      worker_token_env: CPA_WRITER_WORKER_TOKEN
```

Rules:

- intervals: `0s` disabled or 1 minute through 24 hours; default disabled;
- `max_version_retries`: 0–5 additional attempts; default 2;
- all `worker_token_env` names are identical and resolve non-empty;
- `management_key_env` resolves for every Writer operation;
- model-info key fingerprint is exactly 64 lowercase hex and matches exactly one non-empty normalized root key;
- plaintext management key, worker token, or model-info proxy key is invalid in plugin ConfigYAML;
- environment changes take effect only through plugin reconfigure/restart;
- planner selectors, filters, metadata sources, and precedence stay in their respective plugin subtrees;
- timers remain disabled until read-only auth preflight and deployment acceptance pass.

Writer unavailable means writes fail closed. No file, direct-PUT, snapshot-key, manual-provider-key, or derived-auth-index fallback exists.

## 11. Sanitized outcome contract

Writer trigger routes normally return `202`. Synchronous validation/configuration failures and blocked write triggers return their mapped 4xx/5xx status. Asynchronous jobs expose the code and state through Writer status while preserving the originating Core HTTP class internally. Numeric classes below apply directly when a route returns the outcome, including model-info `/ingest` and `writer_blocked`.

| Code | HTTP/Core class | Meaning and required behavior |
|---|---:|---|
| `invalid_request` | 400 | Malformed protocol; no source/body echo |
| `version_conflict` | 409 | Pre-PUT or model-info drift; expose authoritative version only |
| `commit_verification_failed` | 409 | PUT returned but exact authoritative bytes differ; uncertain, block |
| `persisted_runtime_uncertain` | 409 | Persisted bytes but runtime convergence unproven; block, never replay |
| `catalog_too_large` | 413 | Model-info decoded catalog exceeds 8 MiB; preserve cache |
| `catalog_invalid` | 400 | Malformed ingest/base64/catalog; preserve cache |
| `invalid_config` | 422 | Core rejected config; suppress unproven Core detail |
| `provider_credential_unavailable` | 422 | No unique active file-backed callback identity; no proposal/PUT |
| `provider_fetch_invalid` | 400 | Unsafe descriptor/continuation/page; no provider call when detected pre-relay |
| `provider_fetch_failed` | 502/504 | `/api-call` transport, timeout, or non-2xx; discard body |
| `provider_catalog_too_large` | 413 | Page/cumulative provider bytes exceed 8 MiB; no PUT |
| `core_unavailable` | 502 | Loopback failure or unusable response |
| `writer_unconfigured` | 503 | Management/token/origin config unavailable |
| `catalog_key_unavailable` | 503 | Fingerprint does not select exactly one normalized root key |
| `loopback_timeout` | 504 | Bounded pre-PUT timeout after planner inactivity proven |
| `planner_stalled` | asynchronous | Planner inactivity unproven; block until reconcile |
| `startup_reconcile_required` | asynchronous | Initial fail-closed state |
| `writer_blocked` | 409 | Write rejected while blocker remains; include only blocker ID/code |
| `reconcile_failed` | asynchronous | Required live hash/version/instance evidence absent; block remains |

## 12. Acceptance gates

### Module and source gates

- build, test, race-test, vet, and verify every Go module;
- reproducibly build every c-shared plugin artifact and record hashes;
- source-scan production code for forbidden direct config/key-file, management-write, credential HTTP, and legacy JSON/model-channel surfaces;
- prove worker routes reject management-authenticated callers lacking the private token;
- prove responses/logs/status contain no secrets or source excerpts;
- keep generated files, temporary configs, and `.pi/` outside commits.

### Protocol and adversarial gates

- exact snapshot/version vectors and strict JSON/base64 bounds;
- exact and concurrent continuation replay: one receipt succeeds at most once;
- new-attempt, token rotation/reconfigure, shutdown, cross-attempt, cross-generation, snapshot/version/request substitution invalidation;
- multi-channel/page continuation with one bounded live attempt;
- callback-only file-backed identity success and config-only credential fail-closed behavior;
- forced auth-removal race proves no PUT after persistent drift without asserting zero network request;
- provider proxy priority and absence of environment proxy;
- malformed/ambiguous/oversize catalog fail closed without body leakage;
- membership and metadata ownership matrices reject every non-owned path, including comments/styles/tags/order/aliases/plugins/root keys;
- multiple independently valid provider membership sequence changes in one no-scope run are accepted;
- two proposals from one base: first commits, second recomputes from fresh snapshot;
- no stale proposal or continuation replay.

### Runtime and four-plugin gates

- startup blocks writes until all four current ConfigYAML hashes converge;
- non-noop commit persists exact predicted bytes, injects one common fresh epoch, and advances every live plugin sequence/hash;
- no-op creates no PUT, epoch churn, or reconfigure wait;
- stalled planner and post-PUT uncertainty block all writes until evidence-based reconcile;
- acknowledgement, partial status, stale version, wrong instance, or wrong hash cannot clear a blocker;
- reconfigure preserves Writer mutex/queue/instance and unrelated absolute deadlines;
- membership preserves retained metadata/aliases; metadata preserves membership/order/name/alias;
- model-info cache survives epoch reconfigure and failed ingest;
- restart from persisted YAML reproduces membership and metadata;
- direct external PUT race is tested only as detectable residual, never claimed atomic;
- exact fixed Core composition loads all four artifacts and passes management/provider E2E;
- rollback restores pre-deployment artifacts/config/data and is exercised before final production acceptance.

Every exact candidate receives fresh independent functional and adversarial review. Approval must be `approved` or `approved with explicit residual risk` with no actionable finding. Any reviewed-file change invalidates that approval.

## 13. Primary evidence

Authoritative upstream references:

- CLIProxyAPI PR #5246: <https://github.com/router-for-me/CLIProxyAPI/pull/5246>
- CLIProxyAPI PR #5247: <https://github.com/router-for-me/CLIProxyAPI/pull/5247>
- CLIProxyAPI PR #5264: <https://github.com/router-for-me/CLIProxyAPI/pull/5264>
- stock auth callback example: `examples/plugin/host-callback-auth-files/`

Source seams to recheck when revising this contract:

- raw config GET/PUT and persistence normalization;
- management authentication and plugin route dispatch;
- plugin ConfigYAML register/reconfigure behavior;
- host-auth list/get/get-runtime admission rules;
- stock `/api-call` token substitution, proxy priority, timeout, and buffering;
- root proxy API-key normalization;
- plugin watcher/reconfigure completion behavior.

Repository implementation and tests are executable authority for completed slices. This contract remains authority for cross-plugin boundaries and unfinished slices; stale model-channel revision/CAS plans are not implementation authority.
