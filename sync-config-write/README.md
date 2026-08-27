# sync-config-write

Native CPA plugin for serialized configuration orchestration.

Implemented slices:

- direct `plugins.configs.sync-config-write` ConfigYAML validation and environment-only secret indirection;
- fixed no-proxy numeric-loopback CPA client;
- persistent process instance, global queue/worker/status registry, operation coalescing, startup block, bounded retention, deadline-preserving reconfigure, and phase/attempt reporting;
- exact-byte snapshot versioning and base64 planner envelopes;
- path-aware membership and metadata ownership validation;
- serialized full-config PUT, exact reread verification, four-plugin `sync_epoch` injection, runtime convergence evidence, and fail-closed reconciliation;
- five Writer management routes.

Deferred by design: provider `/api-call` continuation relay, model-info catalog fetch/ingest, worker-plugin conversion, and UI. Until the worker routes are converted, startup reconciliation safely remains blocked and write runs cannot complete.

## Build

```sh
env -u GOROOT go test ./... -count=1
go test -race ./... -count=1
go vet ./...
make build-local
```
