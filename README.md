# cpa-plugin-mono

CLIProxyAPI plugin monorepo. Each child directory is an independent plugin; directory name equals plugin ID and artifact name.

| Plugin | Ownership |
|---|---|
| [`auto-pull-models`](./auto-pull-models) | OpenAI-compatible catalog filtering and membership proposals |
| [`model-metadata-sync`](./model-metadata-sync) | Existing-model six-field metadata proposals for explicit OpenAI-compatible and Claude selectors |
| [`model-info`](./model-info) | Read-only detailed model catalog cache and UI |
| [`sync-config-write`](./sync-config-write) | Serialized stock-CPA config orchestration, provider relay, reconcile, and model-info refresh |

Build outputs live under each plugin's `build/plugins/linux/amd64/<plugin-id>.so`.

## Selected-Core four-plugin integration

Run real loopback E2E against fixed reviewed Core composition:

```sh
integration/four-plugin-e2e.sh
```

Environment:

- `CPA_CORE_SOURCE`: CLIProxyAPI source checkout; default `../CLIProxyAPI`. Checkout is never switched or modified.
- `CPA_KEEP_E2E_TMP=1`: retain disposable composition/runtime for diagnosis; default removes it.
- `XDG_CACHE_HOME`: optional parent for disposable test data; defaults to `~/.cache`.

Harness creates a no-hardlink disposable Core clone with direct ancestry through the two approved base patches plus three stable-patch-identical commits. It verifies all five patch IDs, source-checkout immutability, and absence of model-channel/revision/CAS surfaces; uses task-local Go/temp directories; and builds Core plus reproducible versioned c-shared artifacts. Runtime checks cover HTTPS provider membership/metadata through stock `/api-call`, exact full-config PUTs, explicit startup reconcile, independent runtime hashes, exact model-info views, restart persistence, private worker authorization, and missing file-credential/no-PUT gates. Generated credentials stay temporary and must not be supplied from real environments.

## Split migration

Old combined auto-pull config is intentionally not read. Convert provider-name maps into explicit `channels[]` composite selectors, keep filtering in `auto-pull-models`, move metadata sources/overrides to `model-metadata-sync`, then preview both while intervals remain `0`. See both plugin READMEs and config examples.
