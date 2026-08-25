package plugin

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestMetadataPatchWireUsesOperationsAndFieldObjects(t *testing.T) {
	request := metadataPatchRequest{
		Kind: KindOpenAI, Selector: ChannelSelector{Name: "x", BaseURL: "https://x.example/v1"},
		ExpectedRevision: "r1", ExpectedModelNames: []string{"m"},
		Operations: []ModelPatch{{Model: "m", Fields: map[string]FieldPatch{"thinking.levels": {Mode: "replace", Value: []string{"low", "high"}}}}},
	}
	raw, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	for _, required := range []string{`"kind":"openai-compatibility"`, `"expected_revision":"r1"`, `"expected_model_names":["m"]`, `"operations":[`, `"model":"m"`, `"thinking.levels":{"mode":"replace","value":["low","high"]}`} {
		if !strings.Contains(text, required) {
			t.Fatalf("missing %s in %s", required, text)
		}
	}
	if strings.Contains(text, `"models":`) || strings.Contains(text, `"name":"m"`) {
		t.Fatalf("legacy model patch alias in %s", text)
	}
}

func TestBuildModelPatchesUsesBaselineFieldModes(t *testing.T) {
	before := []ModelRef{{Name: "m"}}
	after := []ModelRef{{Name: "m", MaxContextLength: 128000}}
	reports := []ModelMetadataResult{{Model: "m", Fields: []MetadataFieldResult{{Field: "max-context-length", Status: "completed"}}}}
	patches, err := buildModelPatches(before, after, reports)
	if err != nil {
		t.Fatal(err)
	}
	if len(patches) != 1 || patches[0].Model != "m" || patches[0].Fields["max-context-length"].Mode != "if-empty" {
		t.Fatalf("patches=%+v", patches)
	}

	after[0].MaxContextLength = 256000
	reports[0].Fields[0].Status = "upstream"
	patches, err = buildModelPatches(before, after, reports)
	if err != nil {
		t.Fatal(err)
	}
	if patches[0].Fields["max-context-length"].Mode != "replace" {
		t.Fatalf("upstream patch=%+v", patches)
	}
}

func TestBuildModelPatchesSkipsChangesWithoutWritableProvenance(t *testing.T) {
	before := []ModelRef{{Name: "m"}}
	after := []ModelRef{{Name: "m", MaxOutputTokens: 1}}
	reports := []ModelMetadataResult{{Model: "m", Fields: []MetadataFieldResult{{Field: "max-output-tokens", Status: "preserved"}}}}
	patches, err := buildModelPatches(before, after, reports)
	if err != nil {
		t.Fatal(err)
	}
	if len(patches) != 0 {
		t.Fatalf("unexpected patch=%+v", patches)
	}
}

func TestBuildModelPatchesNeverAddsAndRejectsDuplicateModels(t *testing.T) {
	reports := []ModelMetadataResult{{Model: "new", Fields: []MetadataFieldResult{{Field: "max-output-tokens", Status: "override"}}}}
	patches, err := buildModelPatches(nil, []ModelRef{{Name: "new", MaxOutputTokens: 1}}, reports)
	if err != nil || len(patches) != 0 {
		t.Fatalf("new model patch=%+v err=%v", patches, err)
	}
	before := []ModelRef{{Name: "dup"}, {Name: "dup"}}
	reports = []ModelMetadataResult{{Model: "dup", Fields: []MetadataFieldResult{{Field: "max-output-tokens", Status: "override"}}}}
	if patches, err = buildModelPatches(before, []ModelRef{{Name: "dup", MaxOutputTokens: 1}}, reports); err == nil || !strings.Contains(err.Error(), "ambiguous") || len(patches) != 0 {
		t.Fatalf("duplicate model patch=%+v err=%v", patches, err)
	}
}
