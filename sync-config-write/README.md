# sync-config-write

Foundation native CPA plugin for serialized configuration orchestration.

Implemented slice:

- direct `plugins.configs.sync-config-write` ConfigYAML validation and secret environment indirection;
- fixed no-proxy numeric-loopback CPA client;
- persistent process instance, global queue/worker/status registry, operation coalescing, startup block, reconcile admission, bounded status retention, and deadline-preserving reconfigure;
- five Writer management routes;
- injected `Executor` seam returning sanitized `not_yet_implemented` outcomes.

Deferred by design: YAML proposal/ownership/commit engine, provider `/api-call` relay, model-info catalog fetch/ingest, runtime convergence handshake, and UI.

## Build

```sh
env -u GOROOT go test ./... -count=1
go test -race ./... -count=1
go vet ./...
make build-local
```
