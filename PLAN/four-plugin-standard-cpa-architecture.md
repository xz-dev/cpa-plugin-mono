# Four-plugin standard-CPA architecture plan

Status: source-verified plan; no implementation or deployment authorized. Independent review corrections and all user architecture decisions are incorporated.

## 1. Decision and feasibility

The requested architecture is feasible on an otherwise unmodified CLIProxyAPI by adding a standard plugin named `sync-config-write` and routing every cooperating full-config write through the same live Writer instance. The remaining limitations are explicit stock-Core operational boundaries, not unresolved feasibility assumptions.

Target Core composition:

1. start from the fixed release tag `v7.2.142` at `1f53b2eb`;
2. apply exactly the commits in PRs #5246, #5247, and #5264;
3. apply no other Core patch—notably not `07cb171d` and not any model-channel revision/CAS API.

The five required PR commits applied cleanly in order to the fixed baseline:

- #5246: `908c46d2`, `e469ef03`;
- #5247: `eea510a8`;
- #5264: `fd70b12a`, `b2b2d61d`.

Before building, verify that the `v7.2.142` tag still resolves to `1f53b2eb` and fetch the exact five commit objects. Cherry-pick those objects in the listed order onto that fixed commit. If any cherry-pick conflicts, becomes empty, or needs adaptation, stop for a new source review and plan revision—do not change the base, skip an already-equivalent patch by ancestry, or add fixups/unrelated PR-history commits. Record original SHA → resulting SHA and verify stable patch-id equivalence for every patch. The final diff from `1f53b2eb` must consist of exactly those five patches; the clean reference review composite is `/tmp/cpa-four-plugin-selected-review` at `4430edcc`. Any future release-base update requires a separately source-verified plan revision.

### Feasible guarantee

Provider-catalog feasibility has one explicit stock precondition: every selected OpenAI-compatible/Claude catalog identity must have exactly one matching active file-backed CPA auth exposed by `host.auth.list/get_runtime/get`. Ordinary config-only API-key auths are omitted by those callbacks in the selected Core; under the user's callback-only and no-extra-Core-patch constraints they are unsupported and fail closed. No plugin falls back to snapshot keys, user-entered keys, management descriptors, or files.

Within one CPA process, one unchanged live `sync-config-write` instance prevents lost updates among commits routed exclusively through that instance, **provided no stock CPA config PUT/PATCH or other bypass writer overlaps the commit window**:

- snapshot version is one canonical hash of exact CPA GET response bytes;
- commits are serialized by one in-process mutex;
- the writer re-fetches and compares immediately before PUT;
- stale orchestration attempts receive `409 Conflict`; Writer discards the proposal and recomputes from a new snapshot.

This is not a Core-wide transaction lock. Writer artifact/path/version replacement during a commit is prohibited; ordinary Writer reconfigure must update settings without replacing its synchronization identity.

### Unavoidable boundary

Stock CPA exposes unconditional `PUT /v0/management/config.yaml`, not conditional PUT/CAS. Raw GET does not take the management handler lock, and raw PUT validates before acquiring it. Any stock management mutation or external client that bypasses Writer can overlap Writer's GET→PUT window. A post-PUT read can detect an observable final-byte mismatch, but cannot make the transaction atomic against that bypass writer.

Therefore the supported operational contract is:

- plugin-originated writes go only through `sync-config-write`;
- no direct stock config PUT/PATCH may overlap a Writer commit;
- operators SHOULD pause timers and avoid stock config mutations while applying manual changes;
- direct external or Core management writes are outside the serialization guarantee;
- multiple CPA processes sharing one config file are unsupported.

This residual race is documented, not hidden. No additional Core patch is proposed.

## 2. Verified CPA surfaces

Source references are relative to CLIProxyAPI.

### Raw config management

- `internal/api/server_management.go:14-188` registers authenticated `GET` and `PUT /v0/management/config.yaml`.
- `internal/api/handlers/management/config_basic.go:167-182` reads the config file and returns raw bytes as-is with YAML content type and `no-store`.
- `internal/api/handlers/management/config_basic.go:111-163` reads the full PUT body, YAML-decodes it, validates it through `LoadConfigOptional(..., false)`, locks the management handler, writes, reloads, updates `h.cfg`, and only then returns `200`.
- `internal/api/handlers/management/config_basic.go:94-109` normalizes standalone comment indentation and performs truncate/write/fsync/close. The response bytes after PUT may therefore differ from submitted bytes; the post-PUT GET is authoritative.
- GET itself does not acquire the handler mutex and stock PUT has no caller-supplied precondition.

### Management authorization

- `internal/api/handlers/management/handler.go:265-397` requires `Authorization: Bearer <key>` or `X-Management-Key`, including localhost requests.
- CPA may authenticate the plaintext `MANAGEMENT_PASSWORD`, a runtime-local password, or a plaintext value matching the configured bcrypt hash. The YAML `remote-management.secret-key` is normally a bcrypt hash and cannot be reused as the bearer credential.
- No stock plugin-only management bypass exists.

CPA also exposes plugin-host credential callbacks that require no management key:

- `host.auth.list` returns admitted file-backed/runtime-only credential identities and stable `auth_index`;
- `host.auth.get` returns physical credential JSON only when that runtime auth has a backing path;
- `host.auth.get_runtime` returns runtime metadata only for an entry admitted by the same file-backed/runtime-only filter.

`internal/pluginhost/auth_callbacks.go:44-113,135-150,218-259,389-480` shows that stock list/get_runtime omit ordinary config-synthesized API-key auths through `buildHostAuthFileEntry` because they have neither a file path nor `runtime_only`; `get` follows the separate `authPhysicalJSONByIndex` path and rejects an entry without a non-empty readable backing path. Deriving an internal index would not make the three-callback preflight succeed. Therefore the strict callback-only/no-Core-patch architecture supports provider catalog credentials only when exactly one matching active **file-backed** auth is exposed by all three callbacks. Config-only API-key credentials are an explicit unsupported stock case and fail closed; using their unredacted snapshot key or an internally derived index would violate the user's callback requirement. The callbacks are demonstrated by `examples/plugin/host-callback-auth-files/`; they solve provider identity/authentication, not management authorization.

Only Writer calls authenticated Writer/Core management routes and therefore only Writer resolves and retains the plaintext management credential from its configured environment-variable name. With the exact Core composition there is no plugin-local management-auth bypass and no `host.management.*` callback. Planner/model-info plugins have no management-secret setting and may not read a management key file; their internal management handlers unavoidably receive the transient Authorization header from stock dispatch and must ignore it.

### Standard plugin and routing APIs

- `internal/pluginhost/config.go:29-88` preserves each `plugins.configs.<id>` YAML subtree and sends it as `ConfigYAML` on register/reconfigure.
- `internal/pluginhost/management.go:37-151` registers plugin-owned management routes under `/v0/management`; Core routes are reserved.
- `internal/api/server_management.go:240-276` authenticates a management request before dispatching it to the plugin host.
- A plugin can reach another plugin's authenticated management route by ordinary loopback HTTP and supplying the management key.
- `internal/pluginhost/http_bridge.go:108-136` calls `newHTTPClient(nil)`, so stock `host.http.do` can apply only CPA's global proxy; it cannot select the resolved credential's proxy. Writer therefore MUST NOT use `host.http.do` for loopback secrets, and planners do not use it for credential-bound provider catalogs.
- Writer uses a dedicated stdlib loopback client: numeric loopback origin only (`http://127.0.0.1:<port>` or `http://[::1]:<port>`), cloned default transport with `Proxy=nil`, redirects disabled, fixed paths constructed in code, and a 120-second request timeout. Reject non-HTTP schemes, non-loopback hosts, userinfo, origin paths/query/fragment, and caller-selected destinations. This client is only transport to stock CPA HTTP APIs; it introduces no private Core callback or patch.
- `internal/api/handlers/management/api_tools.go:39-223,484-525` implements stock `POST /v0/management/api-call`: `auth_index` selects a runtime credential, `$TOKEN$` is substituted inside Core, and proxy priority is request override → credential → channel config → global → direct, with no environment proxy. Writer uses this route as a bounded provider-fetch relay and never receives the provider token. Source lines 132-159 and 190-192 also expose a stock race: if the auth disappears after callback validation, `authByIndex` may return nil, token substitution leaves literal `$TOKEN$`, and Core can still call the upstream. Callback validation is therefore point-in-time, not atomic with network execution; this plan prevents a proposal/PUT after persistent post-fetch drift but does not claim it can prevent that raced placeholder request without a Core change.
- `internal/access/config_access/provider.go:55-126` trims configured root `api-keys` before matching and accepts `Authorization: Bearer`; Writer fingerprints and sends that same normalized value for model-info.
- `internal/api/middleware/request_logging.go:459-465` excludes management paths from detailed request/response capture; the ordinary Gin access logger records status, latency, client IP, method, and masked path/query only. Planner host-auth callbacks remain synchronous and local to an active plan step.

### Reload behavior and selected PRs

- #5246 adds OpenAI-compatible `max-output-tokens` config/catalog/hash support.
- #5247 preserves HTTP status through nested plugin model execution errors; it is required by the exact Core composition but is not needed for config CAS.
- #5264 improves filesystem completion observation, serializes watcher reloads, and handles config snapshot/hash identity. It does not add conditional management PUT.
- Raw management PUT assigns the management handler's `h.cfg`, but does not synchronously run the broader management-save reload hook; watcher/server/plugin reconfiguration remains asynchronous. A post-PUT GET proves persisted bytes only. Before another scheduled write, Writer must observe a bounded reconfigure/status transition or classify the runtime outcome as uncertain.
- Writer artifact selection (`store.version`, release tag, path) must remain unchanged during a commit. Acceptance tests cover a model commit followed separately—while timers/jobs are stopped—by an administrative Writer-settings reconfigure that preserves the loaded artifact and synchronization identity.

## 3. Plugin responsibilities

### `sync-config-write`

Owns generic, serialized full-config snapshots/commits **and orchestration**. It is the only plugin configured with CPA management authorization.

It does not choose model selectors, desired membership, metadata precedence, or provider catalog semantics. It does understand the generic continuation/fetch envelope and independently enforces operation-specific YAML ownership invariants before commit. For each run it fetches the snapshot, drives the responsible pure planner and stock `/api-call` relay, then version-checks and persists the validated proposal. It does not read, write, rename, watch, or lock the CPA config file; it calls only CPA's official HTTP APIs through the fixed direct loopback client above.

### `auto-pull-models`

Owns OpenAI-compatible membership only:

- find one exact configured channel;
- fetch the upstream model catalog;
- apply include/exclude filters;
- replace only model membership while retaining allowed existing per-model data;
- expose an authenticated, side-effect-free planning route that accepts a Writer snapshot and returns proposed full YAML plus its membership report;
- never call Writer or a CPA config write endpoint itself;
- never modify any `plugins.*` subtree or `sync_epoch`.

It must not enrich metadata.

### `model-metadata-sync`

Owns metadata on models that already exist in the snapshot:

- OpenAI-compatible and Claude exact selectors;
- thinking levels, context/input/output limits, and modalities;
- existing source precedence/provenance rules;
- no add, remove, rename, alias, reorder, or channel creation;
- expose an authenticated, side-effect-free planning route that accepts a Writer snapshot and returns proposed full YAML plus provenance/report data;
- never call Writer or a CPA config write endpoint itself;
- never modify any `plugins.*` subtree or `sync_epoch`.

### `model-info`

Remains read-only. It displays the effective `/v1/models?client_version=1.0.0` catalog and cached/effective views. Because stock CPA exposes no plugin-host callback for the assembled client-facing model catalog and `/v1/models` is protected by the independent proxy `api-keys` middleware, Writer is the only catalog fetcher: a user/manual refresh calls Writer; Writer GETs authoritative config, selects exactly one existing root `api-keys` value by configured lowercase SHA-256 fingerprint, and makes a fixed direct-loopback GET to its own `/v1/models?client_version=1.0.0` with `Authorization: Bearer <normalized-key>`. Writer then sends the size-limited catalog to `model-info`'s private `/ingest` route. `model-info` validates/parses/caches that catalog. It is never configured with, returns, logs, or retains the management credential or proxy API key; stock management dispatch transiently delivers Writer's Authorization header to the handler, which must ignore it. It must not call Writer commit or any CPA write endpoint.

## 4. `sync-config-write` protocol

Routes:

- `POST /v0/management/plugins/sync-config-write/run/auto-pull-models`
- `POST /v0/management/plugins/sync-config-write/run/model-metadata-sync`
- `POST /v0/management/plugins/sync-config-write/model-info/catalog`
- `POST /v0/management/plugins/sync-config-write/reconcile`
- `GET /v0/management/plugins/sync-config-write/status`

Worker routes called only by Writer:

- `POST /v0/management/plugins/auto-pull-models/plan`
- `POST /v0/management/plugins/model-metadata-sync/plan`
- `POST /v0/management/plugins/model-info/ingest`
- `GET /v0/management/plugins/auto-pull-models/writer-status`
- `GET /v0/management/plugins/model-metadata-sync/writer-status`
- `GET /v0/management/plugins/model-info/writer-status`

Worker `writer-status` responses contain exactly `instance_id`, `reconfigure_seq`, `config_sha256`, and planner-only `active_plan`; they contain no settings or secrets.

All routes above are protected by CPA management middleware. Only Writer resolves and retains the management credential. Writer supplies that credential on its loopback calls to Core and internal worker routes; worker plugins have no management-credential setting and do not resolve or persist it. Stock dispatch includes the transient Authorization header in each worker's `ManagementRequest`; every worker must ignore it and must not return, log, or retain it. Planner, ingest, and worker-status routes additionally require a private high-entropy `X-Sync-Config-Writer-Token` resolved by Writer/workers from a dedicated process environment variable whose **name**, not value, is configured in each plugin's ConfigYAML. Thus a management GET/full snapshot does not disclose the token. This additional check prevents an ordinary management-authenticated caller from directly invoking internal workers unless they also possess process-environment access.

### Model-info catalog size contract

The catalog body limit is exactly **8 MiB (8,388,608 bytes)** of raw `/v1/models` response body.

1. Writer's direct loopback client streams at most 8 MiB + 1 byte from `/v1/models`; on the extra byte it closes the response, records sanitized `catalog_too_large`, skips `/ingest`, and leaves `model-info`'s prior cache untouched.
2. Writer base64-frames the accepted raw catalog bytes in JSON for `/ingest`, because stock management JSON sanitization must not alter payload bytes. The 8 MiB limit applies to decoded catalog bytes, not base64/JSON envelope size. The request has exactly one payload field:

```json
{"catalog_base64":"<base64 of exact /v1/models response bytes>"}
```

No URL, method, headers, credential, full config, or caller-selected fetch option is accepted.
3. Core management dispatch necessarily buffers the full encoded `/ingest` request before the handler runs. Therefore this limit is validation and cache protection, not transport-memory protection.
4. `model-info` validates JSON/base64 and independently rejects decoded bytes over 8 MiB before catalog parsing. On malformed, oversize, or parse failure it returns a sanitized error and atomically retains the previous cached catalog; cache replacement occurs only after complete successful parse. Success returns only `{"count":<n>,"catalog_sha256":"<lowercase SHA-256 of decoded bytes>"}`; catalog bytes remain available through model-info's existing read-only catalog/effective views.
5. Neither failure path includes body excerpts, parser source fragments, or keys in responses/logs/status.

### Asynchronous orchestration

All run/catalog orchestration is asynchronous to avoid recursive native host calls from an active management handler:

- a Writer `POST /run/*`, `POST /model-info/catalog`, or `POST /reconcile` validates configuration, enqueues at most one pending job per operation, and normally returns `202` with an opaque `run_id`; blocked write triggers instead return sanitized `409 writer_blocked`, while reconcile remains available;
- one Writer worker goroutine executes all write, model-info, and reconcile jobs globally one at a time;
- `GET /status?run_id=<opaque>` reports only `run_id`, operation, state (`queued|planning|fetching|committing|waiting_reconfigure|reconciling|succeeded|failed|uncertain|blocked`), attempt count, timestamps, versions, changed flag, sanitized error code, blocking run ID, and Writer's own `instance_id`/`reconfigure_seq`/`config_sha256`—never bodies, headers, snapshots, reports containing secrets, or credentials. `run_id` is 128 bits from `crypto/rand` encoded base64url without padding. Omit the query to return the current plus most recent run for each operation. Writer retains at most 32 ordinary completed statuses plus the currently pinned blocker/reconcile status in memory and nothing on disk;
- duplicate timer ticks while that operation is queued/running are coalesced; manual overlap returns the existing `run_id` rather than starting another copy;
- Writer stores one monotonic absolute next-run deadline per periodic operation. `Configure` atomically updates future settings without replacing the queue, commit mutex, `instance_id`, active job, or unchanged deadlines: a `sync_epoch`-only reconfigure preserves every deadline; changing one interval schedules only that operation from `configure_time + new_interval`; disabling removes only its deadline. Expired deadlines enqueue once through normal coalescing. A process restart creates fresh deadlines from successful startup reconciliation; there is no persisted timer catch-up. Each active job keeps an immutable run-scoped settings copy;
- retries occur only for pre-PUT version conflicts and always start from a new snapshot; transport failures after PUT, verification mismatch, or reconfigure uncertainty are terminal `uncertain` outcomes and are never replayed;
- each planner exposes `active_plan` in its token-protected `writer-status`. If Writer's 120-second planner request expires, it never commits that proposal and checks this flag. A cleared flag records failed `loopback_timeout`; true/unavailable records `planner_stalled` and atomically blocks all later manual/scheduled writes. The pure planner has no persistence capability, so a late return cannot commit;
- after verified persistence, Writer waits for evidence that plugin reconfigure reached the persisted config version. For every non-noop commit, Writer owns and injects the same fresh `sync_epoch` (128 bits from `crypto/rand`, base64url without padding) into each of the four existing plugin ConfigYAML subtrees after validating the planner proposal and before predicting persisted bytes. Before PUT it captures Writer's local status tuple and the three workers' token-protected tuples `instance_id`, `reconfigure_seq`, and `config_sha256`; after PUT every unchanged instance must report a greater `reconfigure_seq` and the exact expected SHA-256 for its ConfigYAML containing the new epoch. `instance_id` is a random process-instance identifier; `reconfigure_seq` starts at registration and increments only after a successful Configure; `config_sha256` hashes exact injected ConfigYAML bytes. A same-bytes external reconfigure before PUT cannot satisfy the gate because it lacks the new epoch. Writer reproduces Core's documented `runtimeConfigYAML` normalization and fixture-tests it against the exact target Core. The direct loopback client's 120-second deadline bounds each worker-status request; nonconvergence records `persisted_runtime_uncertain` and atomically blocks later manual/scheduled writes.

### Internal snapshot envelope

Writer calls stock CPA `GET /v0/management/config.yaml` only from its detached orchestration worker and sends planners JSON with base64 framing:

```json
{
  "version": "<64 lowercase hex digits>",
  "config_base64": "<base64 of exact full unredacted response bytes>"
}
```

Core HTML-sanitizes string values in JSON plugin-management responses, so raw YAML cannot safely be transported as a JSON string. Base64 uses only characters unaffected by that sanitizer. Planners decode `config_base64` to recover exact bytes.

Rules:

- hash algorithm is exactly SHA-256 over the decoded raw GET response body;
- version is exactly 64 lowercase hexadecimal digits, with no prefix;
- no newline, YAML, or line-ending normalization before hashing;
- Writer and planners treat `version` as opaque outside the Writer hashing implementation; planners do not independently define a competing algorithm;
- snapshot body and version live only for the current orchestration attempt and are not cached as durable Writer state.

A planner response is a tagged union. Final success returns:

```json
{
  "base_version": "<exact version received>",
  "config_base64": "<base64 of complete proposed YAML bytes>",
  "report": {"changed": true}
}
```

An intermediate response contains `next_fetch` as specified below and no `config_base64`. Writer rejects envelopes containing both or neither. Writer requires echoed `base_version` to equal the attempt version, validates every envelope, and treats `report` as sanitized diagnostics only—not commit authority. Reports may contain counts, selected channel identity, changed model names/fields, and provenance labels, but no headers, credential/auth records, raw YAML, source fragments, or environment values.

### Stock provider-fetch relay

Planners never perform credential-bearing provider HTTP themselves. During an active plan step they resolve and revalidate exactly one active file-backed `auth_index` through `host.auth.list/get_runtime/get`, then may return:

```json
{
  "base_version": "<exact version received>",
  "next_fetch": {
    "request_id": "<opaque per-attempt step id>",
    "kind": "<openai_models|claude_models|modelparams|modelsdev>",
    "selector": {"channel_name": "<OpenAI name>", "base_url": "<normalized snapshot value>"},
    "auth_index": "<validated stable index or empty for public metadata>",
    "method": "GET",
    "url": "https://<snapshot-derived trusted origin>/<fixed catalog path>",
    "header": {"Authorization": "Bearer $TOKEN$"},
    "continuation_base64": "<base64 of secret-free planner continuation state>"
  }
}
```

Writer's continuation request repeats the original `version`, `config_base64`, and scope and adds exactly:

```json
{
  "continuation_base64": "<exact echoed planner state>",
  "fetch_result": {
    "request_id": "<exact requested step id>",
    "status_code": 200,
    "body_base64": "<base64 of exact valid-UTF-8 upstream body returned by Core>"
  }
}
```

For official Anthropic the allowed template is `x-api-key: $TOKEN$` plus fixed `anthropic-version`; public metadata fetches omit `auth_index` and credential headers. Writer accepts only known `kind`, `GET`, HTTPS, no userinfo/fragment, a small header allowlist, `$TOKEN$` placeholders rather than literal credentials, and continuation state no larger than 8 MiB decoded. For OpenAI kinds, `selector` requires exact channel name + normalized base URL; for Claude it requires stable config index + normalized base URL/prefix; public metadata uses an otherwise empty selector. Using `kind`/`selector`, Writer independently resolves the selected snapshot entry and requires exact normalized origin plus an allowlisted fixed catalog path/query shape; public metadata kinds map to fixed compiled origins. Caller- or continuation-selected origins are rejected. It forwards the descriptor through its fixed no-proxy loopback client to stock `POST /v0/management/api-call`, omitting `proxy_url` so Core applies credential/channel/global proxy priority. Core substitutes/refreshes the credential; Writer receives only status, response headers, and body.

Writer accepts only a 2xx upstream result, discards response headers, independently tracks and caps each decoded page and the per-attempt cumulative provider bytes at 8 MiB, then calls the same planner again with the original snapshot/version/scope plus exact base64-framed body bytes, request ID, status, and echoed continuation. Before accepting that body, returning another fetch, or returning the final proposal, the planner repeats list/get_runtime/get identity validation; persistent removal or mismatch discards the body and produces no proposal/PUT. These checks are point-in-time: stock `/api-call` can still issue one request containing literal `$TOKEN$` if auth disappears in the preflight-to-execution window, and an exact remove/restore ABA cannot be proven absent. Claude pagination is otherwise stateless across route calls except for the echoed secret-free continuation. Core's stock `/api-call` has a 60-second timeout and buffers its upstream response before Writer can enforce the 8 MiB cap; this is an explicit stock memory boundary. Stock provider redirects follow Go's ordinary redirect policy; only credential-selected trusted catalog origins are allowed, and no redirect-suppression guarantee is claimed for upstream calls.

### Blocked-state reconciliation

`POST /v0/management/plugins/sync-config-write/reconcile` is the only unblock operation. It is protected by normal CPA management auth, returns `202`, and runs through the same queue/mutex; acknowledgement alone never clears a block.

- For `planner_stalled`, reconcile queries both token-protected planner statuses and clears the block only when both routes respond from live instances with `active_plan: false`.
- For `persisted_runtime_uncertain` or startup, reconcile GETs authoritative raw config, derives exact expected ConfigYAML hashes for all four current plugin subtrees, and requires Writer's local tuple plus all three worker tuples to report those hashes from live instances. It also requires no active planner. Sequence advancement is required when a pre-PUT tuple survives in memory; after process restart, exact current hashes and fresh live instance IDs are the proof.
- Writer starts in `startup_reconcile_required`; no periodic/manual write can run until one automatic reconcile succeeds. Failure remains `blocked`, schedules keep their deadlines but enqueue no write, and operators retry the same reconcile route after repairing credentials/runtime; after successful reconcile, each already-expired operation enqueues exactly once through normal coalescing.
- Reconcile records the authoritative version and evidence timestamps, then atomically clears only the matching blocker under the commit mutex. A concurrent new blocker or config-version change makes reconciliation fail without clearing. No PUT occurs.

### Internal commit service

After a planner returns the proposal envelope, Writer passes this in-memory value to its commit service:

```json
{
  "base_version": "<64 lowercase hex digits>",
  "config_base64": "<base64 of complete proposed YAML bytes>"
}
```

Writer algorithm:

1. reject malformed/missing planner fields, mismatched echoed version, invalid base64, or missing config; any plugin-side size check is post-read because stock Core buffers the full management body before dispatch;
2. acquire one process-local commit mutex whose identity survives ordinary Writer reconfigure;
3. GET current raw CPA config;
4. hash exact current bytes;
5. if hash differs from `base_version`, return typed `version_conflict` with only current version—never current YAML;
6. if proposed bytes exactly equal current bytes, record `changed: false` and skip PUT/epoch/reconfigure handshake;
7. run the operation-specific path-aware ownership validator below; if it reports zero owned-node changes, record `changed: false` and skip PUT even if planner serialization bytes differ; otherwise generate a fresh `sync_epoch` and update only that Writer-owned field in all four plugin subtrees;
8. compute `expectedPersisted = config.NormalizeCommentIndentation(epochAdjustedProposal)` equivalently in plugin code, matching stock `WriteConfig` exactly;
9. PUT the epoch-adjusted proposed YAML to stock CPA;
10. on non-2xx, return a mapped error without echoing request or Core body containing secrets;
11. GET the config again and hash exact post-PUT bytes;
12. require exact byte equality between authoritative GET bytes and `expectedPersisted`; otherwise record `commit_verification_failed` with authoritative version and require a fresh snapshot;
13. using the pre-PUT status tuples already captured by the orchestration job, compute expected ConfigYAML hashes for the authoritative persisted YAML and perform the token-protected post-PUT handshake described above;
14. if convergence is not proven—or a status call times out—record `persisted_runtime_uncertain`, block all later writes except reconcile, and never replay the PUT;
15. release the mutex only after verification/handshake state is recorded.

Writer independently enforces planner ownership; planner correctness is not commit authority. It parses base/proposal as single-document `yaml.Node` trees, rejects duplicate mapping keys and ambiguous aliases/merge keys in mutable paths, and compares every non-owned node path-aware including kind, tag, value, style, anchor/alias target, comments, and sequence/mapping order (ignoring only parser line/column positions):

- membership proposal: every root/channel/plugin node is unchanged except exactly zero or one selected OpenAI-compatible `models` sequence; retained model mappings are node-identical by exact name, removals/reordering are allowed, every new mapping is exactly `{name: <unique non-empty name>}`, and no metadata value is created or changed;
- metadata proposal: channel/model membership, model order/name/alias, and all non-owned nodes are unchanged; only the six declared metadata keys on existing exact-name model mappings may be added, removed, or value-changed;
- both: `plugins.enabled`, `plugins.dir`, all four plugin subtrees, artifact/store/path/version/release identity, root API keys, remote-management fields, and every unrelated channel/config node must be unchanged.

Only after this validation does Writer add `sync_epoch` to the four plugin subtrees. Runtime settings may change only through separately reviewed Writer-owned configuration logic outside model runs; topology/artifact changes are a separately authorized deployment operation with timers stopped. This keeps status routes and instance identity stable throughout the handshake.

Do not use generic semantic YAML equality. It can hide a formatting/comment-only bypass write and has ambiguous behavior around tags, aliases, duplicate keys, ordering, and scalar styles. Unknown keys and ordering remain present because planner transforms edit the snapshot's `yaml.Node` tree rather than marshal a Core struct; the dedicated validator above makes the allowed differences explicit.

The service result is recorded in sanitized run status after authoritative-byte verification and required worker convergence are proven:

```json
{
  "version": "<64 lowercase hex digits of authoritative persisted raw bytes>",
  "changed": true
}
```

Exact byte equality is an immediate no-op fast path. A path-aware result with zero owned-node differences is also a no-op, so planner-only reserialization cannot create formatting/epoch churn. Both skip PUT and reconfigure handshake.

### Outcome and route-error mapping

Writer trigger routes normally return `202`; invalid/unconfigured requests and blocked write triggers may synchronously return their sanitized 4xx/5xx class. Asynchronous outcomes below are exposed as `status.error_code`/state, with originating Core HTTP class retained internally. Numeric HTTP statuses apply directly where a route returns them (for example model-info `/ingest` or `409 writer_blocked`).

- `invalid_request` (HTTP 400 when synchronous validation applies): malformed protocol fields; no YAML or secret echo.
- `version_conflict` (Core class 409): precondition mismatch; include authoritative `version` only; Writer recomputes until its configured retry bound.
- `commit_verification_failed` (Core class 409): PUT returned but authoritative bytes differ from the exactly predicted normalized bytes; include authoritative `version` only and mark outcome uncertain.
- `persisted_runtime_uncertain` (Core class 409): bytes persisted but required worker identity convergence could not be proven; do not replay and block all later writes until evidence-based reconcile succeeds.
- `413 catalog_too_large`: on direct `/ingest`, decoded `/v1/models` body exceeds 8 MiB; Writer uses the same sanitized code in the asynchronous run's failed status. Writer does not call `/ingest` when it detects oversize, and previous cache remains unchanged.
- `400 catalog_invalid`: malformed ingest envelope/base64/catalog JSON; Writer records the same sanitized asynchronous failure code and previous cache remains unchanged.
- plugin-side catalog size rejection is validation/cache protection only; stock host/management paths already buffered the transport body.
- no config-body memory-protection claim is made: stock Core buffers snapshot/commit management bodies before plugin validation; an optional Writer size policy is post-read validation only.
- `invalid_config` (Core class 422): stock CPA rejected YAML/config validation; keep Core detail out of default response/log unless proven secret-free.
- `provider_credential_unavailable` (Core class 422): selected channel has no unique active file-backed identity exposed consistently by list/runtime/get. Detection before a descriptor means no provider call; detection during mandatory post-fetch revalidation discards the fetched body and permits no proposal/PUT, but does not retroactively prevent the documented raced placeholder request.
- `provider_fetch_invalid` (Core class 400): malformed/unsafe planner fetch descriptor, continuation, page progression, or literal credential header; no provider call is made.
- `provider_fetch_failed` (Core class 502/504): stock `/api-call` transport failed, timed out, or returned non-2xx; body/headers are discarded and no PUT occurs.
- `provider_catalog_too_large` (Core class 413): one page or cumulative decoded provider payload exceeds 8 MiB; no PUT occurs.
- `core_unavailable` (Core class 502): loopback transport failure or unusable response.
- `writer_unconfigured` (Core class 503): management credential/token unavailable or invalid loopback origin.
- `catalog_key_unavailable` (Core class 503): model-info key fingerprint is malformed or does not match exactly one non-empty root `api-keys` scalar; no catalog request is made.
- `loopback_timeout` (Core class 504): Writer's fixed no-proxy loopback client exceeded its 120-second deadline. Before PUT this is failed only when planner `active_plan` clears; otherwise use blocking `planner_stalled`. After PUT any timeout becomes `persisted_runtime_uncertain`.
- `planner_stalled`: timed-out planner remains active or cannot prove it stopped; pure proposal is discarded, no PUT occurs, and all writes remain blocked until reconcile proves both planners inactive.
- `startup_reconcile_required`: initial safe state before current authoritative config and all four live ConfigYAML hashes are proven.
- `writer_blocked` (HTTP 409 on write triggers): startup, `planner_stalled`, or persisted runtime uncertainty has not yet passed reconcile; include only blocking run ID/error code.
- `reconcile_failed`: required live status/hash/version evidence was absent or changed; block remains and no PUT occurs.

Never automatically replay an uncertain or interrupted PUT with the same stale base. After reconcile, the next write always starts from a new snapshot.

## 5. Authorization and secret handling

The full YAML is intentionally unredacted. These native plugins are trusted members of the same plugin system.

Required controls:

- no direct config/key-file access;
- Writer's ConfigYAML contains `management_key_env`, `model_info_proxy_api_key_sha256`, and `worker_token_env`; Writer resolves the management/token secrets from process environment and selects the catalog proxy key only from authoritative root `api-keys` by fingerprint;
- plaintext management/token values or a plaintext model-info proxy key are forbidden in the four plugin ConfigYAML subtrees. The exact full snapshot remains intentionally unredacted and contains Core root `api-keys` plus provider/config secrets; trusted planners must not use those outside their declared provider-catalog flow.
- plugins retain resolved secret copies only in process memory and exclude them from status/effective-config responses; the selected catalog proxy key's authoritative source remains ordinary Core root `api-keys` in the CPA config;
- snapshot/commit bodies, headers, Core response bodies, and YAML parse errors containing source fragments are never logged;
- plugin management responses never include full YAML except a token-protected planner proposal's base64 field returned to Writer;
- no YAML, management key, catalog proxy API key, or coordination token in `last`, status, metrics, persistent plugin JSON, temp files, panic text, or error envelopes;
- Writer's credential-bearing Core/worker/model-info/`api-call` loopback calls use only the fixed direct no-proxy client, never `host.http.do`; secrets never appear in URLs/query strings; inbound management access logs contain status, latency, client IP, method, path, and masked query when present, but not bodies or headers;
- planners use `host.auth.list/get_runtime/get` only for active file-backed credentials during plan steps, never log/return/persist physical JSON or snapshot credential values, and return only validated `auth_index` plus `$TOKEN$` header templates. Writer relays those through stock management `/api-call`; Core alone substitutes/refreshes the provider token and selects the credential/channel/global proxy. Every successful credential-bound fetch is followed by planner callback revalidation before its body can contribute to another fetch or proposal. Provider bodies returned to planners are exact base64 and never logged; a stock auth-removal race may expose only the literal placeholder to the trusted origin, never the credential, and no detached/background provider call exists;

The planner plugins receiving a snapshot can read all secrets actually present in CPA YAML—including root `api-keys`; environment indirection prevents only an extra plaintext copy in Writer's subtree and keeps the plaintext management credential and worker token out of YAML. This is the user-selected trusted-planner boundary. Planner code must not consume the root catalog proxy key. `model-info` receives only the size-limited catalog payload and is outside the full-YAML trust boundary; its handler transiently sees the management Authorization header due to stock dispatch but ignores and never retains it.

These controls are cooperative boundaries inside one trusted native-plugin process, not sandboxing: any malicious native plugin could read process environment, memory, or the unredacted snapshot. Acceptance covers the four reviewed plugin implementations and proves that only Writer code resolves the management-secret variable and selects/uses the root catalog proxy key; it does not claim isolation from an arbitrary malicious plugin.

## 6. Writer-coordinated plan/commit loop

Write plugins are pure planners for each invocation; Writer owns every authenticated read/write and retry:

1. manual request or deadline selects `auto-pull-models` or `model-metadata-sync`; Writer first requires startup/block reconciliation to be clear;
2. Writer directly GETs current Core config and computes `base_version`;
3. Writer invokes that plugin's authenticated `/plan` route with base64 snapshot, opaque version, optional selector scope, and private coordination token;
4. planner verifies the token, parses exact bytes into `yaml.Node`, resolves/validates exactly one active file-backed provider identity through `host.auth.list/get_runtime/get`, and either returns a secret-free `next_fetch` or a final proposal;
5. for `next_fetch`, Writer validates the descriptor, invokes stock Core `/api-call`, strips headers/errors, base64-frames accepted body bytes, and invokes the planner continuation; repeat only within page/size bounds;
6. planner transforms only owned YAML nodes and finally echoes `base_version` with complete proposed YAML as base64 plus sanitized report; it performs no persistence or outbound HTTP;
7. Writer validates the envelope and operation-specific structural ownership, then captures pre-PUT status tuples for its reconfigure gate;
8. Writer acquires the commit mutex, re-fetches current config, and compares to `base_version`;
9. on mismatch, Writer discards proposal, continuation, and fetched pages and repeats from step 2, up to `max_version_retries` additional attempts;
10. on match, Writer injects epoch, PUTs, re-GETs, verifies exact predicted normalized bytes, performs the reconfigure handshake, and records the outcome;
11. after the conflict retry bound, Writer records a sanitized failed run and waits for the next deadline/manual invocation.

Do not hold the commit mutex during planner or provider-fetch steps. Do not rebase a serialized proposal or continuation onto a fresh version; rerun from a new snapshot. Planner routes are internal: users trigger Writer `/run/*`, not planners directly.

### Exact selectors

- OpenAI-compatible: trimmed `name` plus normalized `base-url`; duplicates are an error.
- Claude: stable `config_index` in plugin configuration plus normalized effective `base-url` and normalized `prefix`; after locating the index, verify base/prefix still match before modifying.

### Membership transformation

For every desired upstream name:

- if that name already exists, retain its entire YAML model mapping, including its alias and metadata;
- if new, create the minimum mapping `{name: <upstream-name>}` without inventing an alias;
- remove names excluded by the desired membership;
- emit desired upstream order;
- reject duplicate existing names;
- do not write any model metadata field for a newly created model in this plugin.

The `keep_existing_aliases` option is removed. The migration always preserves retained aliases and omits alias on new models, per user decision.

### Metadata transformation

- operate only on exact existing names;
- preserve sequence membership and order;
- update only these six YAML keys: `thinking`, `max-context-length`, `max-input-tokens`, `max-output-tokens`, `input-modalities`, and `output-modalities`; `display-name` is explicitly not owned;
- retain unrelated model keys and comments;
- preserve current values according to existing source precedence and per-field `replace`/`if-empty` decisions;
- reject duplicates/ambiguous selectors;
- re-run all precedence decisions after a version conflict.

## 7. Catalog fetching without the removed model-channel APIs

The current repository depends on excluded `/model-channels` endpoints and must be changed.

### OpenAI-compatible catalogs

Use the Writer snapshot as authority for channel name, base URL, proxy, prefix, headers, and configured credential shape. Provider identity is resolved only through stock plugin-host auth callbacks; network execution is relayed by Writer through stock management `/api-call` so credential-specific proxy policy is honored.

Required fail-closed flow:

1. locate exactly one snapshot channel by exact name + normalized base URL;
2. use `host.auth.list` to match exactly one active available file-backed entry to the selected snapshot channel, then `host.auth.get_runtime` and `host.auth.get` for the same stable index to revalidate runtime/physical provider and base identity; discard physical JSON immediately without using its secret directly;
3. require non-empty path-backed identity and stable `auth_index`; config-only, initially missing, disabled, unavailable, mismatched, or ambiguous credentials fail closed before any descriptor/fetch;
4. return a GET-only fetch descriptor whose HTTPS origin/path and safe headers are derived from that selected snapshot and whose credential value is only `$TOKEN$`;
5. Writer invokes stock `/api-call` with the point-in-time validated `auth_index`; Core substitutes/refreshes token and applies credential/channel/global proxy priority, subject to the documented concurrent-removal placeholder race;
6. after a 2xx body, planner repeats list/get_runtime/get identity validation before parsing and returning the membership proposal; persistent drift discards the body and prevents PUT.

This preserves the established no-user-entered-key behavior while avoiding the source-verified `host.http.do` credential-proxy gap. The excluded `model_channels.go` may be consulted only for migration URL/header logic; it is not an API in target Core.

### Claude catalogs

Use the snapshot to locate the Claude entry by stable configured index and verify normalized base URL + prefix. For every continuation step, repeat file-backed `host.auth.list/get_runtime/get` identity checks, return the next fixed GET descriptor, and let Writer relay it through stock `/api-call`; config-only Claude keys fail closed and no planner performs provider HTTP.

Header construction is explicit and host-sensitive:

- official `api.anthropic.com`: `x-api-key: $TOKEN$` plus `anthropic-version: 2023-06-01`;
- compatible gateways: `Authorization: Bearer $TOKEN$` unless their verified API contract requires the Anthropic key form;
- copy only configured catalog-safe headers from the selected snapshot entry; reject attempts to override Host, credential, or management headers unexpectedly.

Preserve `limit=1000`, `after_id=last_id`, `has_more`, strict cursor progress, a 100-page bound, and the 8 MiB cumulative decoded-body bound. Any missing/drifted auth index, reordered entry identity, disabled/unavailable credential, credential/snapshot mismatch, continuation mismatch, or repeated cursor detected at a planner boundary prevents the next descriptor or final proposal. A concurrent auth removal after a descriptor is returned may still cause stock Core to issue the documented literal-placeholder request; persistent drift is caught on the post-fetch planner call and no PUT follows.

### External metadata sources

`modelparams.dev` and `models.dev` use the same Writer `/api-call` relay with no `auth_index` or credential header, fixed HTTPS origins/paths, and base64-framed bounded bodies. They are not CPA config mutations.

## 8. Configuration ownership

All four plugins decode ordinary settings and secret-environment variable names directly from `ConfigYAML`, which is sourced from `plugins.configs.<id>`. Only Writer resolves management-auth and CPA proxy-read secrets. The planners and `model-info` resolve only the shared worker coordination token; they have no management/proxy credential setting. Remove all external plugin JSON and key-file behavior:

- no `config_file` fields;
- no `GET|PUT .../json` routes;
- no plugin-created config files;
- no management key file fields;
- no direct CPA or plugin configuration/key file reads or mutations (`os.ReadFile`, `os.WriteFile`, `os.Rename`, watchers, or locks against those paths); ordinary embedded/resource reads are not forbidden.

Writer's coordination fields are exact:

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

`core_origin` defaults to the shown numeric loopback origin and is validated as described in §2. Intervals accept `0s` (disabled) or 1 minute through 24 hours; all default to disabled. `max_version_retries` counts additional fresh-snapshot attempts after the first, defaults to 2, and accepts 0–5. The four `worker_token_env` names must be identical and resolve non-empty. `management_key_env` must resolve for every Writer operation. `model_info_proxy_api_key_sha256` is exactly 64 lowercase hex digits and model-info refresh requires it to match exactly one authoritative root `api-keys` scalar **after `strings.TrimSpace`**, matching Core's `internal/access/config_access/provider.go` normalization; hash and send that normalized UTF-8 value, reject empty or duplicate normalized matches. Environment values are resolved into run/configure-scoped memory and are not hot-reloaded merely because the process environment changes; rotate them through explicit plugin reconfigure/restart. Planner selectors/filters and metadata source precedence remain in their own ConfigYAML subtrees.

Model membership/metadata writes are full-config writes through Writer and trigger asynchronous CPA watcher reconfigure. Bootstrap and four-plugin topology/artifact changes belong to the separately authorized deployment operation, not a runtime plugin route. Operators configure Writer's `management_key_env` to the existing Core plaintext management environment variable (normally `MANAGEMENT_PASSWORD`); Writer is the only one of these four implementations that reads it. For model-info, operators choose one existing high-entropy Core root `api-keys` entry and configure only its SHA-256 fingerprint; Writer selects and uses that key solely for the fixed GET `/v1/models?client_version=1.0.0`, while trusted planner code ignores root API keys. Operators put one high-entropy worker coordination token in a second environment variable and configure only its name in all four plugins. Provider catalog credentials require no extra plugin configuration but must already exist as exactly one matching active file-backed CPA auth; planners use `host.auth.list/get_runtime/get`, and Writer relays only validated `auth_index`/`$TOKEN$` templates through stock `/api-call`. Config-only API keys fail closed. No worker has management/proxy-key fields, and plaintext management/token/proxy values are invalid inside plugin ConfigYAML. Plugin UIs trigger Writer run/catalog routes; they do not mutate configuration directly.

Before enabling timers, each configured provider selector must pass a read-only file-backed list/runtime/get preflight; config-only credentials keep the affected operation unavailable. Writer unavailable/unconfigured means all writes fail closed; no direct-management-PUT, snapshot-key, derived-index, or file fallback is allowed.

## 9. Implementation slices

Each functional slice uses one branch, focused checks, review, commit, merge, and branch deletion before the next slice. Do not deploy as part of these slices.

### Slice 1 — `sync-config-write`

Files/modules:

- add independent `sync-config-write/go.mod`, command ABI scaffold, internal service, management routes, config parser, README, Makefile;
- implement Writer-only model-info catalog proxy using one existing CPA proxy API key, with a hard-coded GET-only path, and private worker-token authentication;
- reuse existing native ABI scaffolding; Writer uses the fixed no-proxy loopback client for Core/worker routes and stock management `/api-call` for planner-requested provider fetches; planners use only active-step file-backed `host.auth.list/get_runtime/get` and never perform outbound HTTP;
- use stdlib `crypto/sha256`, `encoding/hex`, `sync.Mutex`, `net/http`, and existing `gopkg.in/yaml.v3`.

Checks:

- exact-byte SHA-256/version vector;
- fixed-loopback validation rejects non-loopback/userinfo/path/query/fragment origins, disables redirects and proxying, and proves management/proxy keys are never sent to a configured CPA global proxy;
- 120-second timeout against an active planner: a deliberately stalled mock proves `active_plan`/`planner_stalled` blocks all writes, a late pure proposal cannot commit, and reconcile alone clears only after both planners are inactive;
- stock `/api-call` relay accepts only validated GET/HTTPS descriptors, literal `$TOKEN$` templates, bounded continuation/page data, and file-backed credential-selected `auth_index`; tests prove credential/channel/global proxy priority, no environment proxy, stripped response headers/errors, and no provider secret reaches Writer/planner envelopes;
- a forced auth removal between callback preflight and stock `/api-call` demonstrates the exact residual: Core may send literal `$TOKEN$`; non-2xx becomes `provider_fetch_failed`, while a synthetic 2xx is discarded when post-fetch callback revalidation still fails, and neither path performs PUT. Tests must not assert atomic preflight/execution or zero network attempts;
- config-only OpenAI/Claude API-key fixtures are absent from list/get/runtime and fail closed as `provider_credential_unavailable`; no snapshot-key/index-derivation fallback occurs;
- snapshot base64-decodes to exact full YAML and hashes exact decoded bytes;
- two orchestration attempts from one base: first success, second typed pre-PUT conflict and fresh planner/fetch recomputation;
- stale base causes no PUT;
- invalid Core config maps to `422` without body leakage;
- reject plaintext management/token/proxy fields; require valid environment-variable names/non-empty resolved values and an exact lowercase SHA-256 proxy-key selector that matches one Core-normalized (`TrimSpace`) root key;
- Writer ConfigYAML fixtures prove only secret-environment names and the proxy-key fingerprint are present, never plaintext values; full-snapshot fixtures prove plaintext management/token values are absent, acknowledge the selected catalog key's ordinary root `api-keys` presence, and verify planner code never reads that root key while internal worker calls authenticate with the environment-resolved token;
- reject planner calls missing or mismatching the private Writer token;
- verify model-info `/ingest` rejects missing/mismatched Writer token, applies the 8 MiB decoded-body limit, atomically preserves the previous cache on oversize/malformed/parse failure, and is never configured with or passed the proxy API key/full config; its transient management Authorization header is ignored and not retained;
- post-PUT bytes equal the exact predicted `NormalizeCommentIndentation(epochAdjustedProposal)` output;
- non-noop commit injects one fresh identical `sync_epoch` into exactly the four plugin subtrees; exact-byte or zero-owned-delta noops inject none and perform no PUT;
- pre-PUT unrelated/same-config reconfigure increments sequence but cannot satisfy the new-epoch ConfigYAML hash gate;
- path-aware membership/metadata validators reject every non-owned node change, including plugin/topology/artifact, root API key, remote-management, alias, unrelated channel, comment/style/tag/order, duplicate-key, and ambiguous-alias mutations before PUT;
- any post-PUT byte mismatch records uncertain `commit_verification_failed`;
- Writer rejects >8 MiB decoded catalog bodies before `/ingest`, model-info independently returns `413 catalog_too_large` for >8 MiB before parse, and every catalog failure preserves the previous cache;
- malformed envelope/base64/catalog returns sanitized `400 catalog_invalid` and preserves the previous cache;
- base64 ingestion reproduces exact accepted catalog bytes;
- repeated short-interval non-noop commits preserve a longer unchanged operation's absolute deadline; interval changes affect only their own deadline and produce no duplicate tick;
- startup reconcile blocks writes until all four current ConfigYAML hashes are proven; persisted uncertainty cannot clear by acknowledgement, stale version, partial status, or wrong instance/hash; planner-stalled recovery requires both `active_plan` values false;
- no direct CPA/plugin config or key-file APIs in plugin source.

Reviewer required.

### Slice 2 — YAML node and protocol helpers

Duplicate the small base64/version envelope and YAML-node helpers in the two planner modules by default. Writer owns all Core/sibling HTTP orchestration; planners and `model-info` do not implement a Writer client. Add a shared module only if implementation proves enough non-trivial repeated pure logic to justify the extra dependency/workspace surface.

Checks:

- unknown keys/comments survive model-sequence edits;
- path-aware ownership comparison includes kind/tag/value/style/anchor/alias/comments/order and ignores only parser positions;
- membership validator permits only membership semantics; metadata validator permits only six fields on existing models; root/plugin/credential/unrelated mutations fail closed;
- selector ambiguity, duplicate mapping/model keys, and mutable-path alias/merge ambiguity fail closed;
- bounded typed version-conflict recomputation loop;
- no stale serialized document or continuation replay.

Reviewer required.

### Slice 3 — migrate `auto-pull-models`

- replace `/model-channels` inventory/catalog/reconcile calls;
- decode injected ConfigYAML;
- use Writer-provided snapshots and return pure membership proposals from `/plan`;
- require exactly one matching active file-backed credential through list/runtime/get, reject config-only credentials, and return secret-free fetch descriptors; Writer performs stock `/api-call` relay;
- delete JSON/key-file persistence and stale revision code/tests/routes.

Checks:

- retained models preserve aliases, metadata, and unrelated YAML keys;
- new models contain only name;
- removal/order/filter behavior;
- version conflict causes Writer to invoke the membership planner again against a fresh snapshot;
- no metadata fields change;
- fail closed when the Writer-provided snapshot/planning request is unavailable or invalid;
- no management/proxy credential setting or environment resolution; only `worker_token_env` is resolved.

Reviewer required.

### Slice 4 — migrate `model-metadata-sync`

- replace model-channel descriptor/catalog/patch APIs;
- keep enrichment and provenance engine;
- return pure owned-field YAML proposals from `/plan`;
- delete JSON/key-file persistence and obsolete revision tests/routes.

Checks:

- membership/name/alias/order unchanged;
- six-field ownership only;
- OpenAI and Claude host-auth identity checks plus bounded `/api-call` continuation/pagination paths;
- precedence/golden provenance remains valid;
- version conflict causes Writer to invoke the planner again against fresh existing values;
- fail closed when the Writer-provided snapshot/planning request is unavailable or invalid;
- no management/proxy credential setting or environment resolution; only `worker_token_env` is resolved.

Reviewer required.

### Slice 5 — standardize `model-info`

- decode injected ConfigYAML;
- remove config/API key file access;
- fetch the effective catalog only when Writer invokes the private `/ingest` route with a base64-framed payload no larger than 8 MiB decoded; never initiate a Writer call and never configure, return, log, or retain the CPA proxy API key or transient management Authorization header;
- convert model-info `GET .../catalog` to return the current cache only; UI refresh calls Writer `POST .../model-info/catalog`, polls Writer status, then reloads catalog/effective views;
- keep all model-info management resources read-only.

Checks:

- no writer commit calls or outbound Writer/Core fetches;
- catalog/cache and effective fallback behavior preserved across `sync_epoch` reconfigure; instance/status identity changes only on process restart;
- secrets absent from responses/logs/status;
- no management/proxy credential setting or environment resolution; only `worker_token_env` is resolved.

Reviewer required because configuration/runtime behavior changes.

### Slice 6 — four-plugin contract, docs, and Core build recipe

- replace stale PLAN and mock revision/CAS E2E;
- update root and plugin READMEs;
- document exact Core commit recipe and external-writer boundary;
- add an E2E harness that loads all four plugins against exact target Core and a temporary config.

Acceptance scenarios:

1. two planner proposals from one version: Writer commits the first, discards the second after a version mismatch, re-invokes its planner, and preserves the first change;
2. membership sync does not erase metadata; metadata sync does not change membership/aliases/order;
3. Writer PUT causes target Core to persist model changes, inject one common fresh `sync_epoch`, and eventually increment all four plugins' reconfigure sequence with expected ConfigYAML hashes; a separate stopped-job administrative Writer-settings reconfigure preserves Writer instance/mutex identity;
4. no plugin accesses CPA or plugin configuration/key files directly;
5. external direct PUT during the Writer window is detected when observable but is explicitly not claimed atomic;
6. restart from persisted YAML produces the same model membership/metadata;
7. planner routes reject a caller that has valid CPA management auth but lacks the private Writer coordination token;
8. model-info receives only the size-limited effective catalog through Writer; it has no management/proxy-key/full-config setting, ignores the transient management Authorization header, and never returns, logs, or retains those secrets;
9. all timers are paused by default and no deployment occurs;
10. repeated 5-minute membership commits cannot postpone an unchanged 1-hour metadata deadline and do not duplicate ticks;
11. startup, planner-stalled, and persisted-runtime blocks reject writes until the exact reconcile predicates pass; acknowledgement alone never clears;
12. Writer rejects planner proposals that change any non-owned path even when the planner report claims success;
13. provider fetch relay honors credential-specific proxying, uses only file-backed callback-validated auth index/`$TOKEN$` templates, base64-frames pages, and never exposes a provider token to Writer; a forced preflight-to-execution removal may reach the trusted provider with literal `$TOKEN$` but cannot produce a PUT while post-fetch drift remains observable;
14. a matching file-backed auth passes list/runtime/get preflight, while the same selector with only a config-synthesized API key fails closed without snapshot-key or derived-index fallback.

Reviewer required, followed by final review of the combined architecture.

## 10. Verification gates

Before implementation merge:

- build/test each Go module;
- verify exact target CPA build from fixed `v7.2.142` (`1f53b2eb`) with exactly the five selected PR commits and no other patch;
- verify no direct CPA/plugin config or key-file reads/mutations in production code;
- verify planner routes require the independent Writer coordination token in addition to CPA management middleware;
- verify model-info has no key/file settings and Writer's catalog proxy accepts only the fixed GET `/v1/models?client_version=1.0.0` operation;
- verify no `07cb171d`, revision, or Core CAS assumption remains in docs/tests;
- verify management access logs contain status, latency, client IP, method, path, and masked query when present, but no request/response bodies or headers; verify direct loopback planner/worker/`api-call` requests produce no detailed Gin-context capture;
- run race tests for Writer commit serialization, deadline-preserving reconfigure, reconcile, shutdown, and unchanged synchronization identity;
- verify plugin reload does not close the native library while a commit/planner call is active;
- executable-check stalled planner blocking/reconcile and stock `/api-call` credential/proxy/timeout/size/auth-removal-race behavior; confirm no credential-bound provider path uses `host.http.do`, and assert no PUT—not no network request—after the forced race;
- confirm the working tree and commits contain no secrets or generated temporary config.

Residual boundaries that verification must preserve:

- stock full-config PUT uses validate, truncate/write, and file fsync rather than atomic rename; the Writer protocol limits cooperative lost updates but does not claim crash-atomic persistence;
- an external bypass mutation or environment credential rotation after PUT may leave persistence proven but subsequent status/reconfigure confirmation unavailable; record `persisted_runtime_uncertain`, never replay, repair credentials, then require evidence-based reconcile against a new authoritative snapshot;
- host-auth callback validation and stock `/api-call` lookup are non-atomic. Concurrent removal may send literal `$TOKEN$` to the already allowlisted trusted provider; persistent drift is rejected post-fetch and cannot produce a PUT, but exact remove/restore ABA and zero-network-attempt prevention require Core hardening outside this plan.

Deployment is a separate operation and requires explicit user authorization.

## 11. Resolved management-auth topology

The user's correction is valid for **provider credentials**: stock CPA gives plugins `host.auth.list`, `host.auth.get`, and `host.auth.get_runtime`, but source verification shows ordinary config-only API-key auths cannot pass the required three-callback preflight. To honor both callback-only credentials and no additional Core patch, planners require exactly one matching active file-backed auth; Writer relays its secret-free `$TOKEN$` descriptor through stock management `/api-call`, whose Core implementation performs token substitution and credential-aware proxy selection. Preflight is explicitly point-in-time: persistent post-fetch drift prevents every proposal/PUT, while the documented literal-placeholder network race remains a stock residual rather than an impossible atomicity claim.

For configuration writes, only `sync-config-write` holds CPA management authorization:

- Writer fetches snapshots, relays provider fetch descriptors to stock `/api-call`, and performs Core PUT/GET verification;
- Writer invokes authenticated planner routes using its own loopback management credential;
- `auto-pull-models` and `model-metadata-sync` receive the snapshot, compute proposals, and return them; they do not initiate authenticated management calls and never configure, resolve, return, log, or retain the management credential; their handlers ignore the transient stock-dispatch Authorization header;
- Writer fetches the fixed `/v1/models?client_version=1.0.0` path through its no-proxy direct loopback client with its existing CPA proxy API key and delivers only an accepted ≤8 MiB decoded, base64-framed catalog payload to `model-info`'s token-protected `/ingest` route;
- users/timers trigger Writer `/run/*` routes, not planner routes directly;
- all internal worker calls (`/plan` and model-info `/ingest`) require the separate coordination token as well as CPA management middleware.

This matches the exact stock boundary: `host.auth.*` validates provider identities, stock management `/api-call` executes credential/proxy-aware provider requests, `/v1/models` uses existing proxy `api-keys`, and no stock `host.management.*` or assembled-catalog callback exists.

Other resolved decisions:

- aliases are always preserved for retained models; new models omit alias; `keep_existing_aliases` is removed;
- Core base is fixed `v7.2.142` (`1f53b2eb`) plus exactly the five selected commits from PRs #5246/#5247/#5264; changing the base requires separate source review and plan revision.

No implementation, build publication, or deployment is authorized by this plan.

## 12. Sources

Primary source, production, and PR metadata verified on 2026-08-26:

- https://github.com/router-for-me/CLIProxyAPI/pull/5246
- https://github.com/router-for-me/CLIProxyAPI/pull/5247
- https://github.com/router-for-me/CLIProxyAPI/pull/5264
- CPA example `examples/plugin/host-callback-auth-files/`
- established plugin references: https://github.com/magicvr/cpa-grok-panel, https://github.com/markhuangai/cpa-plugin-model-router, and https://github.com/Autsunset/cpa-quota-estimator
- read-only production inspection at `root@100.94.238.35:/root/CPA` (plugin inventory and credential-indirection shape only; no secrets recorded)
- current cpa-plugin-mono implementation under `auto-pull-models/`, `model-metadata-sync/`, and `model-info/`

The current repository's `PLAN/auto-pull-models-split-acceptance.md` and `PLAN/tests/split-plugins-e2e.py` encode the superseded model-channel revision design and must not be used as implementation authority after this plan.

Independent review evidence: `/home/xz/.pi/agent/sessions/--home-xz-Code-ai-cpa-plugin-mono--/subagent-artifacts/outputs/54a32686-3ed9-49e2-8788-116dff998990/plan-review-oracle.md`.
