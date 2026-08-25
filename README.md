# cpa-plugin-mono

CLIProxyAPI plugin monorepo. Each child directory is an independent plugin; directory name equals plugin ID and artifact name.

| Plugin | Ownership |
|---|---|
| [`auto-pull-models`](./auto-pull-models) | OpenAI-compatible catalog filtering and atomic membership reconcile only |
| [`model-metadata-sync`](./model-metadata-sync) | Existing-model metadata enrichment for explicit OpenAI-compatible and Claude selectors |
| [`model-info`](./model-info) | Existing model information UI; unchanged by split |

Build outputs live under each plugin's `build/plugins/linux/amd64/<plugin-id>.so`.

Run the split-plugin management contract check with:

```sh
PLAN/tests/split-plugins-e2e.sh
```

It exercises metadata-before-membership, stale revision rejection, metadata retention, and idempotent follow-up sync against a shared mock core.

## Split migration

Old combined auto-pull config is intentionally not read. Convert provider-name maps into explicit `channels[]` composite selectors, keep filtering in `auto-pull-models`, move metadata sources/overrides to `model-metadata-sync`, then preview both while intervals remain `0`. See both plugin READMEs and config examples.
