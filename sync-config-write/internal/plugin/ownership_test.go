package plugin

import (
	"strings"
	"testing"
)

const ownershipBaseYAML = `api-keys:
  - root-secret
remote-management:
  allow-remote: false
plugins:
  enabled: true
  dir: plugins
  configs:
    sync-config-write:
      enabled: true
      priority: 1
      core_origin: http://127.0.0.1:8317
      sync_epoch: old
    auto-pull-models:
      enabled: true
      priority: 2
      worker_token_env: TOKEN
      sync_epoch: old
    model-metadata-sync:
      enabled: true
      priority: 3
      worker_token_env: TOKEN
      sync_epoch: old
    model-info:
      enabled: true
      priority: 4
      worker_token_env: TOKEN
      sync_epoch: old
openai-compatibility:
  - name: first
    base-url: https://first.example/v1
    models:
      - name: keep
        alias: kept-alias
        max-context-length: 100
      - name: remove
        alias: removed-alias
  - name: second
    base-url: https://second.example/v1
    models:
      - name: untouched
        alias: untouched-alias
claude-api-key:
  - api-key: claude-secret
    models:
      - name: claude-keep
        alias: claude-alias
`

func TestMembershipOwnershipAllowedAndForbiddenMatrix(t *testing.T) {
	multipleProviders := strings.Replace(strings.Replace(ownershipBaseYAML, "      - name: remove\n        alias: removed-alias\n", "", 1), "      - name: untouched\n        alias: untouched-alias", "      - name: second-new", 1)
	allowed := map[string]string{
		"remove reorder and add minimal": strings.Replace(ownershipBaseYAML, `      - name: keep
        alias: kept-alias
        max-context-length: 100
      - name: remove
        alias: removed-alias`, `      - name: new-model
      - name: keep
        alias: kept-alias
        max-context-length: 100`, 1),
		"multiple configured providers": multipleProviders,
	}
	for name, proposed := range allowed {
		t.Run(name, func(t *testing.T) {
			changed, err := validateOwnership(OperationAutoPull, []byte(ownershipBaseYAML), []byte(proposed))
			if err != nil || !changed {
				t.Fatalf("changed=%v err=%v", changed, err)
			}
		})
	}

	for name, proposed := range map[string]string{
		"retained metadata":                strings.Replace(ownershipBaseYAML, "max-context-length: 100", "max-context-length: 101", 1),
		"multiple providers plus metadata": strings.Replace(multipleProviders, "max-context-length: 100", "max-context-length: 101", 1),
		"new model alias": strings.Replace(ownershipBaseYAML, `      - name: remove
        alias: removed-alias`, `      - name: remove
        alias: removed-alias
      - name: new-model
        alias: forbidden`, 1),
		"root api key":      strings.Replace(ownershipBaseYAML, "root-secret", "changed-secret", 1),
		"plugin topology":   strings.Replace(ownershipBaseYAML, "dir: plugins", "dir: elsewhere", 1),
		"plugin artifact":   strings.Replace(ownershipBaseYAML, "priority: 2", "priority: 9", 1),
		"remote management": strings.Replace(ownershipBaseYAML, "allow-remote: false", "allow-remote: true", 1),
		"unrelated channel": strings.Replace(ownershipBaseYAML, "https://second.example/v1", "https://evil.example/v1", 1),
		"comment":           strings.Replace(ownershipBaseYAML, "    base-url: https://first.example/v1", "    # changed\n    base-url: https://first.example/v1", 1),
		"style":             strings.Replace(ownershipBaseYAML, "name: first", `name: "first"`, 1),
		"tag":               strings.Replace(ownershipBaseYAML, "name: first", "name: !!str first", 1),
		"order":             strings.Replace(ownershipBaseYAML, "  - name: first\n    base-url: https://first.example/v1", "  - base-url: https://first.example/v1\n    name: first", 1),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := validateOwnership(OperationAutoPull, []byte(ownershipBaseYAML), []byte(proposed)); err == nil {
				t.Fatal("expected rejection")
			}
		})
	}
}

func TestMetadataOwnershipAllowedAndForbiddenMatrix(t *testing.T) {
	allowed := strings.Replace(ownershipBaseYAML, "        max-context-length: 100", `        max-context-length: 101
        max-input-tokens: 90
        max-output-tokens: 50
        input-modalities: [text, image]
        output-modalities: [text]
        thinking:
          type: levels
          levels: [low, high]`, 1)
	allowed = strings.Replace(allowed, "        alias: claude-alias", "        alias: claude-alias\n        max-context-length: 200", 1)
	changed, err := validateOwnership(OperationMetadataSync, []byte(ownershipBaseYAML), []byte(allowed))
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}

	for name, proposed := range map[string]string{
		"membership added": strings.Replace(ownershipBaseYAML, "      - name: remove", "      - name: added\n      - name: remove", 1),
		"membership reordered": strings.Replace(ownershipBaseYAML, `      - name: keep
        alias: kept-alias
        max-context-length: 100
      - name: remove
        alias: removed-alias`, `      - name: remove
        alias: removed-alias
      - name: keep
        alias: kept-alias
        max-context-length: 100`, 1),
		"name":           strings.Replace(ownershipBaseYAML, "name: keep", "name: renamed", 1),
		"alias":          strings.Replace(ownershipBaseYAML, "kept-alias", "changed-alias", 1),
		"display name":   strings.Replace(ownershipBaseYAML, "        alias: kept-alias", "        alias: kept-alias\n        display-name: forbidden", 1),
		"provider field": strings.Replace(ownershipBaseYAML, "https://first.example/v1", "https://changed.example/v1", 1),
		"plugin config":  strings.Replace(ownershipBaseYAML, "worker_token_env: TOKEN", "worker_token_env: OTHER", 1),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := validateOwnership(OperationMetadataSync, []byte(ownershipBaseYAML), []byte(proposed)); err == nil {
				t.Fatal("expected rejection")
			}
		})
	}
}

func TestOwnershipPermitsUnchangedAliasesAndMergesOutsideMutablePaths(t *testing.T) {
	unrelated := `unrelated:
  defaults: &defaults
    value: one
  copied: *defaults
  merged:
    <<: *defaults
    extra: true
`
	base := unrelated + ownershipBaseYAML
	proposed := strings.Replace(base, `      - name: keep
        alias: kept-alias
        max-context-length: 100
      - name: remove
        alias: removed-alias`, `      - name: new-model
      - name: keep
        alias: kept-alias
        max-context-length: 100`, 1)
	changed, err := validateOwnership(OperationAutoPull, []byte(base), []byte(proposed))
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
}

func TestMetadataOwnershipRejectsKeyIdentityAmbiguity(t *testing.T) {
	for name, proposed := range map[string]string{
		"style changed": strings.Replace(ownershipBaseYAML, "        max-context-length: 100", `        "max-context-length": 101`, 1),
		"tag duplicate": strings.Replace(ownershipBaseYAML, "        max-context-length: 100", "        max-context-length: 100\n        !owned max-context-length: 101", 1),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := validateOwnership(OperationMetadataSync, []byte(ownershipBaseYAML), []byte(proposed)); err == nil {
				t.Fatal("expected rejection")
			}
		})
	}

	baseQuoted := strings.Replace(ownershipBaseYAML, "        max-context-length: 100", `        "max-context-length": 100`, 1)
	proposedQuoted := strings.Replace(baseQuoted, `        "max-context-length": 100`, `        "max-context-length": 101`, 1)
	if changed, err := validateOwnership(OperationMetadataSync, []byte(baseQuoted), []byte(proposedQuoted)); err != nil || !changed {
		t.Fatalf("preserved existing key identity should allow value update: changed=%v err=%v", changed, err)
	}
}

func TestOwnershipRejectsDuplicateMergeAndMutableAlias(t *testing.T) {
	for name, proposed := range map[string]string{
		"duplicate": strings.Replace(ownershipBaseYAML, "    base-url: https://first.example/v1", "    name: duplicate\n    base-url: https://first.example/v1", 1),
		"merge":     strings.Replace(ownershipBaseYAML, "      - name: keep", "      - <<: {name: merged}\n        name: keep", 1),
		"alias":     strings.Replace(ownershipBaseYAML, "      - name: keep", "      - &model {name: keep}\n      - *model\n      - name: keep", 1),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := validateOwnership(OperationAutoPull, []byte(ownershipBaseYAML), []byte(proposed)); err == nil {
				t.Fatal("expected rejection")
			}
		})
	}
}

func TestOwnershipReturnsZeroForSerializationOnlyDifference(t *testing.T) {
	proposed := strings.ReplaceAll(ownershipBaseYAML, "\n", "\r\n")
	changed, err := validateOwnership(OperationAutoPull, []byte(ownershipBaseYAML), []byte(proposed))
	if err != nil || changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
}
