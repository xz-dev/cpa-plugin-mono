# Auto Pull Metadata Source Fallback and Provenance

## Story

Actor: CPA operator configuring an OpenAI-compatible provider.

Need: Select an ordered list of exact metadata sources and see which source supplied each model field.

Value: Avoid provider-name guessing and make catalog enrichment auditable.

## Confirmed Rules

1. A source identity is fully qualified:
   - `modelparams.dev/<provider>/<authType>`
   - `models.dev/<provider>`
2. The same provider name on different websites is a different source.
3. `modelparams.dev` is authoritative. `models.dev` only fills fields still missing.
4. `api_key` and `subscription` are separate modelparams.dev sources.
5. Each CPA provider owns its own ordered source list.
6. Lookup never leaves the configured source list.
7. Resolution is per model field. Later sources fill gaps but do not overwrite earlier values.
8. Manual overrides remain last and must be reported as the final source.
9. Every model report lists each supported field, final value, exact source, fill status, or unresolved reason.

## Acceptance Examples

### AP-1: Primary source plus secondary completion

Given source order:

- `modelparams.dev/openai/subscription`
- `modelparams.dev/openai/api_key`
- `models.dev/openrouter`

And a model whose thinking levels exist in the first source while token limits exist only in models.dev,
when preview runs,
then thinking levels use the subscription source,
context/output limits use the models.dev source,
and each field reports its exact source and completion status.

### AP-2: Auth variants are distinct

Given both `modelparams.dev/openai/subscription` and `modelparams.dev/openai/api_key` contain the same model with different levels,
when subscription appears first,
then subscription data wins and api_key data does not overwrite it.

### AP-3: Later sources only fill gaps

Given an earlier source supplies a field and a later source supplies a different value,
when preview runs,
then the earlier value remains.
A later source may still supply another missing field.

### AP-4: No out-of-list lookup

Given an unconfigured catalog provider contains a matching model but configured sources do not,
when preview runs,
then the matching unconfigured provider is ignored
and unresolved fields report that configured sources supplied no value.

### AP-5: UI selection and provenance

Given metadata catalogs are reachable,
when the operator edits a CPA provider,
then the UI offers only catalog-discovered fully qualified source IDs,
allows adding/removing/reordering one source at a time,
and renders every model's field-level provenance after preview.

## Verification Seam

- Automated: Go tests around `Service.Preview`, config parsing, and catalog lookup.
- Manual UI: embedded management page against a local mock management API after automated behavior passes.
