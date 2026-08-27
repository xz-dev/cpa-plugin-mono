package plugin

import (
	"strings"
	"testing"
)

func TestRuntimeConfigYAMLMatchesSelectedCoreFixture(t *testing.T) {
	const raw = `plugins:
  configs:
    sync-config-write:
      core_origin: http://127.0.0.1:8317
      sync_epoch: epoch-a
    auto-pull-models:
      enabled: true
      priority: 7
      worker_token_env: TOKEN
      sync_epoch: epoch-a
    model-metadata-sync:
      enabled: false
      priority: 0
      worker_token_env: TOKEN
      sync_epoch: epoch-a
    model-info:
      enabled: true
      # fixture comment
      worker_token_env: TOKEN
      sync_epoch: epoch-a
`
	want := map[string]string{
		"sync-config-write":   "core_origin: http://127.0.0.1:8317\nsync_epoch: epoch-a\nenabled: false\npriority: 0\n",
		"auto-pull-models":    "enabled: true\npriority: 7\nworker_token_env: TOKEN\nsync_epoch: epoch-a\n",
		"model-metadata-sync": "enabled: false\npriority: 0\nworker_token_env: TOKEN\nsync_epoch: epoch-a\n",
		"model-info":          "enabled: true\n# fixture comment\nworker_token_env: TOKEN\nsync_epoch: epoch-a\npriority: 0\n",
	}
	got, err := runtimeConfigHashes([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	for id, expectedYAML := range want {
		if got[id] != configVersion([]byte(expectedYAML)) {
			t.Fatalf("%s hash=%s want=%s for %q", id, got[id], configVersion([]byte(expectedYAML)), expectedYAML)
		}
	}
}

func TestInjectSyncEpochUsesSameFreshEpochAndProtectsOtherFields(t *testing.T) {
	epoch := "0123456789abcdefghij-_"
	adjusted, hashes, err := injectSyncEpoch([]byte(ownershipBaseYAML), epoch)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(adjusted), "sync_epoch: "+epoch) != 4 {
		t.Fatalf("adjusted config did not inject same epoch four times:\n%s", adjusted)
	}
	for _, id := range pluginIDs {
		if hashes[id] == "" {
			t.Fatalf("missing hash for %s", id)
		}
	}
	withoutEpoch, err := stripPluginEpochs(adjusted)
	if err != nil {
		t.Fatal(err)
	}
	baseWithoutEpoch, err := stripPluginEpochs([]byte(ownershipBaseYAML))
	if err != nil {
		t.Fatal(err)
	}
	if !nodesIdentical(baseWithoutEpoch, withoutEpoch) {
		t.Fatal("epoch injection changed protected nodes")
	}
}

func TestNormalizeCommentIndentationMatchesSelectedCoreFixture(t *testing.T) {
	raw := []byte("root:\n  # nested\n  value: x\n\t# tabbed\n# root\n")
	want := "root:\n# nested\n  value: x\n# tabbed\n# root\n"
	if got := string(normalizeCommentIndentation(raw)); got != want {
		t.Fatalf("got=%q want=%q", got, want)
	}
}
