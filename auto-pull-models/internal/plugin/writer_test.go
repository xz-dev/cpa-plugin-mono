package plugin

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"gopkg.in/yaml.v3"
)

const sampleConfig = `# top comment stays
host: ""
port: 8317
remote-management:
  allow-remote: true
  secret-key: "$2a$10$existingbcryptvalue"
openai-compatibility:
  - name: ZCode
    base-url: "http://zcode-proxy:8080/v1"
    api-keys-entries:
      - api-key: sk-live-internal
        auth-index: "1"
    models:
      - name: glm-5.3
        alias: "glm-5.3"
        max-context-length: 1048576
      - name: glm-4.6
        alias: "glm-4.6"
  - name: Other
    base-url: "http://other:9/v1"
    models:
      - name: keep-me
        alias: "keep-me"
`

func writeTemp(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestWriteModelsFileReplacesOnlyTargetProvider(t *testing.T) {
	path := writeTemp(t, sampleConfig)
	err := writeModelsFile(path, map[string][]ModelRef{
		"ZCode": {
			{Name: "glm-5.3", Alias: "glm-5.3", MaxContextLength: 200000, MaxInputTokens: 180000, MaxOutputTokens: 20000, Thinking: &ThinkingConfig{Levels: []string{"low", "high", "max"}}},
			{Name: "glm-5.3[1m]", Alias: "glm-5.3[1m]", MaxContextLength: 1048576},
		},
	})
	if err != nil {
		t.Fatalf("writeModelsFile: %v", err)
	}

	raw, _ := os.ReadFile(path)
	text := string(raw)

	// secret-key and comments survive untouched
	if !strings.Contains(text, "secret-key: \"$2a$10$existingbcryptvalue\"") {
		t.Fatalf("secret-key corrupted:\n%s", text)
	}
	if !strings.Contains(text, "# top comment stays") {
		t.Fatalf("top comment lost:\n%s", text)
	}
	// sibling provider untouched
	if !strings.Contains(text, "name: keep-me") {
		t.Fatalf("sibling provider models lost:\n%s", text)
	}
	// api-key entry preserved
	if !strings.Contains(text, "sk-live-internal") {
		t.Fatalf("api key entry lost:\n%s", text)
	}

	var doc struct {
		OpenAICompatibility []struct {
			Name   string     `yaml:"name"`
			Models []ModelRef `yaml:"models"`
		} `yaml:"openai-compatibility"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("reparse: %v", err)
	}
	if len(doc.OpenAICompatibility) != 2 {
		t.Fatalf("provider count changed: %+v", doc.OpenAICompatibility)
	}
	var zcode *struct {
		Name   string     `yaml:"name"`
		Models []ModelRef `yaml:"models"`
	}
	for i := range doc.OpenAICompatibility {
		if doc.OpenAICompatibility[i].Name == "ZCode" {
			zcode = &doc.OpenAICompatibility[i]
		}
	}
	if zcode == nil {
		t.Fatal("ZCode provider missing after write")
	}
	if len(zcode.Models) != 2 {
		t.Fatalf("ZCode models = %d, want 2: %+v", len(zcode.Models), zcode.Models)
	}
	if zcode.Models[0].Name != "glm-5.3" || zcode.Models[0].MaxContextLength != 200000 || zcode.Models[0].MaxInputTokens != 180000 || zcode.Models[0].MaxOutputTokens != 20000 {
		t.Fatalf("model[0] wrong: %+v", zcode.Models[0])
	}
	if zcode.Models[0].Thinking == nil || len(zcode.Models[0].Thinking.Levels) != 3 {
		t.Fatalf("thinking lost: %+v", zcode.Models[0].Thinking)
	}
	if zcode.Models[1].Name != "glm-5.3[1m]" || zcode.Models[1].MaxContextLength != 1048576 {
		t.Fatalf("model[1] wrong: %+v", zcode.Models[1])
	}

	entries, _ := os.ReadDir(filepath.Dir(path))
	baks := 0
	for _, e := range entries {
		if strings.Contains(e.Name(), "auto-pull-tmp") {
			t.Fatalf("temp file leaked: %s", e.Name())
		}
		if strings.Contains(e.Name(), backupSuffix) {
			baks++
		}
	}
	if baks != 1 {
		t.Fatalf("backups = %d, want 1", baks)
	}
}

func TestWriteModelsFileKeepsInodeAndCapsBackups(t *testing.T) {
	path := writeTemp(t, sampleConfig)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Skip("no inode on this OS")
	}
	inode := stat.Ino

	for i := 0; i < 12; i++ {
		if err := writeModelsFile(path, map[string][]ModelRef{
			"ZCode": {{Name: fmtName(i), Alias: fmtName(i)}},
		}); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}

	info, err = os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	stat, ok = info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatal("lost inode stat")
	}
	if stat.Ino != inode {
		t.Fatalf("inode changed: %d -> %d", inode, stat.Ino)
	}

	matches, err := filepath.Glob(path + backupSuffix + "*")
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != maxBackups {
		t.Fatalf("backups = %d, want %d: %v", len(matches), maxBackups, matches)
	}
}

func fmtName(i int) string {
	return "m-" + strconv.Itoa(i)
}

func TestWriteModelsFileUnknownProviderErrors(t *testing.T) {
	path := writeTemp(t, sampleConfig)
	err := writeModelsFile(path, map[string][]ModelRef{
		"Nope": {{Name: "x"}},
	})
	if err == nil {
		t.Fatal("expected error for unknown provider")
	}
	// original untouched on error
	raw, _ := os.ReadFile(path)
	if !strings.Contains(string(raw), "keep-me") {
		t.Fatal("config modified on failed write")
	}
}

func TestCompileConfigWriteModeValidation(t *testing.T) {
	if _, err := parseFileConfig([]byte(`{"write_mode":"file"}`)); err != nil {
		t.Fatalf("file mode rejected: %v", err)
	}
	if _, err := parseFileConfig([]byte(`{"write_mode":"weird"}`)); err == nil {
		t.Fatal("invalid write_mode accepted")
	}
	cfg, err := parseFileConfig([]byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.WriteMode != WriteModeAPI {
		t.Fatalf("default write_mode = %s, want api", cfg.WriteMode)
	}
}
