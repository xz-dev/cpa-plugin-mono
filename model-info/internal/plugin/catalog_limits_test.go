package plugin

import (
	"bytes"
	"fmt"
	"runtime"
	"strings"
	"testing"
)

func catalogWithModels(count int) []byte {
	var raw strings.Builder
	raw.WriteString(`{"models":[`)
	for index := range count {
		if index > 0 {
			raw.WriteByte(',')
		}
		fmt.Fprintf(&raw, `{"id":"m%d"}`, index)
	}
	raw.WriteString(`]}`)
	return []byte(raw.String())
}

func catalogWithModel(extra string) []byte {
	return []byte(`{"models":[{"id":"model"` + extra + `}]}`)
}

func repeatedJSONStrings(count int, value string) string {
	return strings.TrimSuffix(strings.Repeat(`"`+value+`",`, count), ",")
}

func expectCatalogValidity(t *testing.T, raw []byte, valid bool) {
	t.Helper()
	_, err := parseCatalog(raw)
	if valid && err != nil {
		t.Fatalf("valid catalog rejected: %v", err)
	}
	if !valid && err == nil {
		t.Fatal("invalid catalog accepted")
	}
}

func TestCatalogModelCountLimits(t *testing.T) {
	expectCatalogValidity(t, catalogWithModels(maxCatalogModels), true)
	expectCatalogValidity(t, catalogWithModels(maxCatalogModels+1), false)
}

func TestCatalogStructuralComplexityLimits(t *testing.T) {
	var exactMembers strings.Builder
	for index := 0; index < maxCatalogObjectMembers-1; index++ {
		fmt.Fprintf(&exactMembers, `,"x%d":null`, index)
	}
	expectCatalogValidity(t, catalogWithModel(exactMembers.String()), true)
	expectCatalogValidity(t, catalogWithModel(exactMembers.String()+`,"over":null`), false)

	exactArray := repeatedJSONStrings(maxCatalogArrayElements, "x")
	expectCatalogValidity(t, catalogWithModel(`,"extra":[`+exactArray+`]`), true)
	expectCatalogValidity(t, catalogWithModel(`,"extra":[`+exactArray+`,"x"]`), false)

	structuralCatalog := func(fullArrays, finalElements int) []byte {
		var raw strings.Builder
		raw.WriteString(`{"models":[]`)
		for index := range fullArrays {
			fmt.Fprintf(&raw, `,"x%d":[`, index)
			raw.WriteString(strings.TrimSuffix(strings.Repeat("0,", maxCatalogArrayElements), ","))
			raw.WriteByte(']')
		}
		if finalElements >= 0 {
			fmt.Fprintf(&raw, `,"x%d":[`, fullArrays)
			raw.WriteString(strings.TrimSuffix(strings.Repeat("0,", finalElements), ","))
			raw.WriteByte(']')
		}
		raw.WriteByte('}')
		return []byte(raw.String())
	}
	// Root {"models":[]} scans 3 values/members; every full array field scans
	// 4098; final field scans 2 plus its scalar elements.
	exactFinalElements := maxCatalogScannedValues - 3 - 63*(maxCatalogArrayElements+2) - 2
	expectCatalogValidity(t, structuralCatalog(63, exactFinalElements), true)
	expectCatalogValidity(t, structuralCatalog(63, exactFinalElements+1), false)

	expectCatalogValidity(t, catalogWithModel(`,"`+strings.Repeat("k", maxCatalogObjectKeyBytes)+`":null`), true)
	expectCatalogValidity(t, catalogWithModel(`,"`+strings.Repeat("k", maxCatalogObjectKeyBytes+1)+`":null`), false)
	expectCatalogValidity(t, catalogWithModel(`,"extra":{"models":[`+repeatedJSONStrings(maxCatalogArrayElements, "x")+`]}`), true)
	expectCatalogValidity(t, catalogWithModel(`,"extra":{"id":"`+strings.Repeat("x", maxCatalogDisplayNameBytes)+`"}`), true)
}

func TestCatalogRelevantFieldLimits(t *testing.T) {
	for name, field := range map[string]struct {
		name  string
		limit int
	}{
		"id":           {"id", maxCatalogModelIDBytes},
		"slug":         {"slug", maxCatalogModelIDBytes},
		"display name": {"display_name", maxCatalogDisplayNameBytes},
		"visibility":   {"visibility", maxCatalogShortStringBytes},
	} {
		t.Run(name, func(t *testing.T) {
			identity := `"id":"model",`
			if field.name == "id" || field.name == "slug" {
				identity = ""
			}
			expectCatalogValidity(t, []byte(`{"models":[{`+identity+`"`+field.name+`":"`+strings.Repeat("x", field.limit)+`"}]}`), true)
			expectCatalogValidity(t, []byte(`{"models":[{`+identity+`"`+field.name+`":"`+strings.Repeat("x", field.limit+1)+`"}]}`), false)
		})
	}

	reasoning := func(count, effortBytes int) []byte {
		entry := `{"effort":"` + strings.Repeat("x", effortBytes) + `"}`
		return catalogWithModel(`,"supported_reasoning_levels":[` + strings.TrimSuffix(strings.Repeat(entry+",", count), ",") + `]`)
	}
	expectCatalogValidity(t, reasoning(maxCatalogReasoningLevels, maxCatalogShortStringBytes), true)
	expectCatalogValidity(t, reasoning(maxCatalogReasoningLevels+1, 1), false)
	expectCatalogValidity(t, reasoning(1, maxCatalogShortStringBytes+1), false)

	for _, field := range []string{"input_modalities", "output_modalities"} {
		expectCatalogValidity(t, catalogWithModel(`,"`+field+`":[`+repeatedJSONStrings(maxCatalogModalities, strings.Repeat("x", maxCatalogShortStringBytes))+`]`), true)
		expectCatalogValidity(t, catalogWithModel(`,"`+field+`":[`+repeatedJSONStrings(maxCatalogModalities+1, "x")+`]`), false)
		expectCatalogValidity(t, catalogWithModel(`,"`+field+`":["`+strings.Repeat("x", maxCatalogShortStringBytes+1)+`"]`), false)
	}
}

func TestComplexityFailurePreservesCache(t *testing.T) {
	t.Setenv("TEST_WRITER_TOKEN", "worker-secret")
	service := New()
	if err := service.Configure(validConfigYAML()); err != nil {
		t.Fatal(err)
	}
	if response := service.HandleManagement(request("POST", ingestPath, "worker-secret", ingestBody(catalogWithModels(1)))); response.StatusCode != 200 {
		t.Fatalf("baseline ingest=%d %s", response.StatusCode, response.Body)
	}
	overLimit := catalogWithModel(`,"visibility":"` + strings.Repeat("x", maxCatalogShortStringBytes+1) + `"`)
	if response := service.HandleManagement(request("POST", ingestPath, "worker-secret", ingestBody(overLimit))); response.StatusCode != 400 || string(response.Body) != `{"error_code":"catalog_invalid"}` {
		t.Fatalf("over-limit ingest=%d %s", response.StatusCode, response.Body)
	}
	if cached := service.Last(); cached.Count != 1 || cached.Models[0].ID != "m0" {
		t.Fatalf("failed ingest changed cache: %+v", cached)
	}
}

func TestCatalogComplexityAllocationBound(t *testing.T) {
	padToLimit := func(raw []byte) []byte {
		if len(raw) > maxCatalogBytes {
			t.Fatalf("fixture exceeds byte limit: %d", len(raw))
		}
		return append(raw, bytes.Repeat([]byte(" "), maxCatalogBytes-len(raw))...)
	}
	measure := func(raw []byte) (uint64, error) {
		runtime.GC()
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		_, err := parseCatalog(raw)
		runtime.ReadMemStats(&after)
		return after.TotalAlloc - before.TotalAlloc, err
	}

	overLimit := padToLimit(catalogWithModels(447352))
	overAllocated, err := measure(overLimit)
	if err == nil {
		t.Fatal("over-limit catalog accepted")
	}
	nearLimit := padToLimit(catalogWithModels(maxCatalogModels))
	nearAllocated, err := measure(nearLimit)
	if err != nil {
		t.Fatalf("near-limit catalog rejected: %v", err)
	}
	const practicalAllocationCeiling = 128 << 20
	if overAllocated > practicalAllocationCeiling || nearAllocated > practicalAllocationCeiling {
		t.Fatalf("allocation ceiling exceeded: over=%d near=%d ceiling=%d", overAllocated, nearAllocated, practicalAllocationCeiling)
	}
	t.Logf("8 MiB allocation evidence: over-limit=%d bytes, accepted-near-limit=%d bytes", overAllocated, nearAllocated)
}
