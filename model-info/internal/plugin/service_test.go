package plugin

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func validConfigYAML() []byte {
	return []byte("enabled: true\npriority: 0\nstore:\n  schema-version: 1\n  id: model-info\n  name: Model Info\n  description: Read-only model catalog viewer\n  author: xz-dev\n  version: 0.1.0\n  release-tag: v0.1.0\n  repository: https://github.com/xz-dev/cpa-plugin-mono\n  tags: [models, diagnostics]\n  source-id: official\n  source-name: CPA Plugin Store\n  source-url: https://plugins.example.invalid/index.yaml\n  install:\n    type: github-release\nworker_token_env: TEST_WRITER_TOKEN\nsync_epoch: epoch-a\n")
}

func request(method, path, token string, body []byte) pluginapi.ManagementRequest {
	headers := http.Header{"Authorization": []string{"Bearer management-secret-must-be-ignored"}}
	if token != "" {
		headers.Set(workerTokenHeader, token)
	}
	return pluginapi.ManagementRequest{Method: method, Path: path, Headers: headers, Body: body}
}

func ingestBody(raw []byte) []byte {
	body, _ := json.Marshal(ingestRequest{CatalogBase64: base64.StdEncoding.EncodeToString(raw)})
	return body
}

func TestStrictConfigStatusReconfigureCacheAndShutdown(t *testing.T) {
	t.Setenv("TEST_WRITER_TOKEN", "worker-secret")
	for name, raw := range map[string]string{
		"missing":                "sync_epoch: x\n",
		"unknown":                "worker_token_env: TEST_WRITER_TOKEN\ninterval: 1h\n",
		"duplicate":              "worker_token_env: TEST_WRITER_TOKEN\nworker_token_env: OTHER\n",
		"duplicate lifecycle":    "enabled: true\nenabled: false\nworker_token_env: TEST_WRITER_TOKEN\n",
		"folded key":             "WORKER_TOKEN_ENV: TEST_WRITER_TOKEN\n",
		"folded scalar":          "worker_token_env: >-\n  TEST_WRITER_TOKEN\n",
		"alias":                  "worker_token_env: &token TEST_WRITER_TOKEN\n",
		"aliased value":          "store: &store {version: 0.1.0}\nworker_token_env: TEST_WRITER_TOKEN\nsync_epoch: *store\n",
		"merge":                  "worker_token_env: TEST_WRITER_TOKEN\n<<: {sync_epoch: x}\n",
		"store anchor":           "store: &store {version: 0.1.0}\nworker_token_env: TEST_WRITER_TOKEN\n",
		"store merge":            "store:\n  <<: {version: 0.1.0}\nworker_token_env: TEST_WRITER_TOKEN\n",
		"store duplicate":        "store:\n  version: 0.1.0\n  version: 0.2.0\nworker_token_env: TEST_WRITER_TOKEN\n",
		"store folded duplicate": "store:\n  source-url: https://one.invalid\n  SOURCE-URL: https://two.invalid\nworker_token_env: TEST_WRITER_TOKEN\n",
		"store nested duplicate": "store:\n  install:\n    type: direct\n    TYPE: github-release\nworker_token_env: TEST_WRITER_TOKEN\n",
		"store custom tag":       "store: !include manifest.yaml\nworker_token_env: TEST_WRITER_TOKEN\n",
		"store custom scalar":    "store:\n  version: !env VERSION\nworker_token_env: TEST_WRITER_TOKEN\n",
		"multiple docs":          "worker_token_env: TEST_WRITER_TOKEN\n---\nsync_epoch: x\n",
		"legacy config":          "worker_token_env: TEST_WRITER_TOKEN\nconfig_file: x.json\n",
		"management":             "worker_token_env: TEST_WRITER_TOKEN\nmanagement_key_env: KEY\n",
		"plaintext":              "worker_token_env: TEST_WRITER_TOKEN\nworker_token: secret\n",
		"api key":                "worker_token_env: TEST_WRITER_TOKEN\napi_key: secret\n",
		"empty sync epoch map":   "worker_token_env: TEST_WRITER_TOKEN\nsync_epoch: {}\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseConfig([]byte(raw)); err == nil {
				t.Fatal("expected rejection")
			}
		})
	}
	if _, err := parseConfig(validConfigYAML()); err != nil {
		t.Fatalf("selected-Core store-backed ConfigYAML rejected: %v", err)
	}
	t.Setenv("TEST_WRITER_TOKEN", "")
	if _, err := parseConfig(validConfigYAML()); err == nil {
		t.Fatal("empty token accepted")
	}
	t.Setenv("TEST_WRITER_TOKEN", "worker-secret")

	service := New()
	if err := service.Configure(validConfigYAML()); err != nil {
		t.Fatal(err)
	}
	routes := service.ManagementRoutes()
	want := []string{http.MethodPost + " " + ingestPath, http.MethodGet + " " + writerStatusPath, http.MethodGet + " " + catalogPath, http.MethodGet + " " + lastPath, http.MethodGet + " " + effectivePath}
	for index, route := range routes.Routes {
		if got := route.Method + " " + route.Path; got != want[index] {
			t.Fatalf("route[%d]=%q", index, got)
		}
	}
	unauthorized := service.HandleManagement(request(http.MethodGet, writerStatusPath, "", nil))
	if unauthorized.StatusCode != http.StatusUnauthorized || string(unauthorized.Body) != `{"error_code":"unauthorized"}` || strings.Contains(string(unauthorized.Body), "management-secret") {
		t.Fatalf("unauthorized=%d %s", unauthorized.StatusCode, unauthorized.Body)
	}
	statusResponse := service.HandleManagement(request(http.MethodGet, writerStatusPath, "worker-secret", nil))
	var status map[string]json.RawMessage
	if err := json.Unmarshal(statusResponse.Body, &status); err != nil || len(status) != 3 {
		t.Fatalf("status=%s err=%v", statusResponse.Body, err)
	}
	var typed WorkerStatus
	_ = json.Unmarshal(statusResponse.Body, &typed)
	sum := sha256.Sum256(validConfigYAML())
	decodedID, err := base64.RawURLEncoding.DecodeString(typed.InstanceID)
	if err != nil || len(decodedID) != 16 || typed.ReconfigureSeq != 1 || typed.ConfigSHA256 != hex.EncodeToString(sum[:]) || strings.Contains(string(statusResponse.Body), "worker-secret") {
		t.Fatalf("status=%+v body=%s", typed, statusResponse.Body)
	}

	catalog := []byte(`{"models":[{"slug":"provider/model-a","context_window":1000}]}`)
	if response := service.HandleManagement(request(http.MethodPost, ingestPath, "worker-secret", ingestBody(catalog))); response.StatusCode != http.StatusOK {
		t.Fatalf("ingest=%d %s", response.StatusCode, response.Body)
	}
	if err := service.Configure([]byte(strings.Replace(string(validConfigYAML()), "epoch-a", "epoch-b", 1))); err != nil {
		t.Fatal(err)
	}
	if service.Last().Count != 1 {
		t.Fatal("successful reconfigure cleared cache")
	}
	if err := service.Configure([]byte("worker_token_env: TEST_WRITER_TOKEN\nunknown: true\n")); err == nil || service.Last().Count != 1 {
		t.Fatal("failed reconfigure changed cache")
	}
	service.Shutdown()
	if service.Last().Count != 1 {
		t.Fatal("shutdown cleared read-only cache")
	}
	if response := service.HandleManagement(request(http.MethodGet, writerStatusPath, "worker-secret", nil)); response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status after shutdown=%d", response.StatusCode)
	}
	if response := service.HandleManagement(request(http.MethodPost, ingestPath, "worker-secret", ingestBody(catalog))); response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("ingest after shutdown=%d", response.StatusCode)
	}
}

func TestIngestReceiptCacheViewsPrecedenceAndReadOnlyCatalog(t *testing.T) {
	t.Setenv("TEST_WRITER_TOKEN", "worker-secret")
	service := New()
	if err := service.Configure(validConfigYAML()); err != nil {
		t.Fatal(err)
	}
	raw := []byte(`{"models":[
		{"id":"zproxy/glm-5.3","slug":" zproxy/glm-5.3 ","display_name":"GLM","context_window":272000,"max_context_window":921000,"max_input_tokens":900000,"max_tokens":131072,"max_output_tokens":120000,"max_completion_tokens":110000,"supported_reasoning_levels":[{"effort":"low","description":"Low"},{"effort":"max"}],"input_modalities":["text","image"],"output_modalities":["text"],"visibility":"list","description":"rich field","priority":100,"service_tiers":[]},
		{"slug":"gpt-output","context_window":2000,"max_context_window":1800,"max_output_tokens":100,"max_completion_tokens":90},
		{"slug":"gpt-completion","context_window":3000,"max_completion_tokens":50},
		{"slug":"gpt-fallback","context_window":4000}
	]}`)
	response := service.HandleManagement(request(http.MethodPost, ingestPath, "worker-secret", ingestBody(raw)))
	if response.StatusCode != http.StatusOK {
		t.Fatalf("ingest=%d %s", response.StatusCode, response.Body)
	}
	var receipt map[string]json.RawMessage
	if err := json.Unmarshal(response.Body, &receipt); err != nil || len(receipt) != 2 {
		t.Fatalf("receipt=%s err=%v", response.Body, err)
	}
	var typed ingestReceipt
	_ = json.Unmarshal(response.Body, &typed)
	sum := sha256.Sum256(raw)
	if typed.Count != 4 || typed.CatalogSHA256 != hex.EncodeToString(sum[:]) {
		t.Fatalf("receipt=%+v", typed)
	}
	cached := service.Last()
	if cached.Models[0].ID != "zproxy/glm-5.3" || cached.Models[0].Provider != "zproxy" || cached.Models[0].MaxInput != 900000 || cached.Models[0].MaxTokens != 131072 || len(cached.Models[0].Levels) != 2 || cached.Models[1].MaxInput != 1800 || cached.Models[1].MaxTokens != 100 || cached.Models[2].MaxTokens != 50 {
		t.Fatalf("cached=%+v", cached)
	}
	effective := service.Effective()
	if effective.Models[3].MaxInput != 4000 || effective.Models[3].MaxTokens != 4000 || effective.Models[3].MaxSource != "fallback-context" || effective.Models[0].MaxSource != "upstream" {
		t.Fatalf("effective=%+v", effective)
	}
	for _, path := range []string{catalogPath, lastPath, effectivePath} {
		view := service.HandleManagement(request(http.MethodGet, path, "", nil))
		if view.StatusCode != http.StatusOK || !bytes.Contains(view.Body, []byte(`"count":4`)) {
			t.Fatalf("view %s=%d %s", path, view.StatusCode, view.Body)
		}
	}
	before := service.Last()
	wrongMethod := service.HandleManagement(request(http.MethodPost, catalogPath, "worker-secret", ingestBody([]byte(`{"models":[]}`))))
	if wrongMethod.StatusCode != http.StatusNotFound || service.Last().Count != before.Count {
		t.Fatal("catalog route was not read-only")
	}
}

func TestExactIngestEnvelope(t *testing.T) {
	t.Setenv("TEST_WRITER_TOKEN", "worker-secret")
	service := New()
	if err := service.Configure(validConfigYAML()); err != nil {
		t.Fatal(err)
	}
	valid := ingestBody([]byte(`{"models":[]}`))
	envelope, err := decodeExactIngestRequest(valid)
	if err != nil || envelope.CatalogBase64 != "eyJtb2RlbHMiOltdfQ==" {
		t.Fatalf("valid envelope=%+v err=%v", envelope, err)
	}
	hugeKey := []byte(`{"` + strings.Repeat("k", maxIngestBodyBytes-16) + `":"x"}`)
	for name, raw := range map[string][]byte{
		"missing":        []byte(`{}`),
		"wrong":          []byte(`{"wrong":"e30="}`),
		"folded":         []byte(`{"CATALOG_BASE64":"e30="}`),
		"duplicate":      []byte(`{"catalog_base64":"e30=","catalog_base64":"e30="}`),
		"additional":     []byte(`{"catalog_base64":"e30=","extra":true}`),
		"non-string":     []byte(`{"catalog_base64":null}`),
		"trailing":       []byte(`{"catalog_base64":"e30="} {}`),
		"invalid UTF-8":  append([]byte(`{"catalog_base64":"`), 0xff, '"', '}'),
		"huge first key": hugeKey,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := decodeExactIngestRequest(raw); err == nil {
				t.Fatal("invalid envelope accepted")
			}
		})
	}
	allocated, response := measureIngestAllocation(service, hugeKey)
	if response.StatusCode != http.StatusBadRequest || string(response.Body) != `{"error_code":"catalog_invalid"}` {
		t.Fatalf("huge-key response=%d %s", response.StatusCode, response.Body)
	}
	const hugeKeyAllocationCeiling = 64 << 20
	if allocated > hugeKeyAllocationCeiling {
		t.Fatalf("huge-key allocation=%d ceiling=%d", allocated, hugeKeyAllocationCeiling)
	}
	t.Logf("full handleIngest huge first key: body=%d allocated=%d ceiling=%d", len(hugeKey), allocated, hugeKeyAllocationCeiling)
}

func measureIngestAllocation(service *Service, body []byte) (uint64, pluginapi.ManagementResponse) {
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	response := service.HandleManagement(request(http.MethodPost, ingestPath, "worker-secret", body))
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc, response
}

func TestMalformedIngestEnvelopeAllocationBoundAndCachePreservation(t *testing.T) {
	t.Setenv("TEST_WRITER_TOKEN", "worker-secret")
	service := New()
	if err := service.Configure(validConfigYAML()); err != nil {
		t.Fatal(err)
	}
	if response := service.HandleManagement(request(http.MethodPost, ingestPath, "worker-secret", ingestBody([]byte(`{"models":[{"slug":"stable"}]}`)))); response.StatusCode != http.StatusOK {
		t.Fatalf("baseline ingest=%d %s", response.StatusCode, response.Body)
	}
	prefix := []byte(`{"catalog_base64":"e30=",`)
	member := []byte(`"x":0,`)
	malformed := make([]byte, 0, maxIngestBodyBytes)
	malformed = append(malformed, prefix...)
	for len(malformed)+len(member)+1 <= maxIngestBodyBytes {
		malformed = append(malformed, member...)
	}
	malformed[len(malformed)-1] = '}'
	malformed = append(malformed, bytes.Repeat([]byte(" "), maxIngestBodyBytes-len(malformed))...)

	allocated, response := measureIngestAllocation(service, malformed)
	if response.StatusCode != http.StatusBadRequest || string(response.Body) != `{"error_code":"catalog_invalid"}` {
		t.Fatalf("malformed response=%d %s", response.StatusCode, response.Body)
	}
	if cached := service.Last(); cached.Count != 1 || cached.Models[0].ID != "stable" {
		t.Fatalf("malformed ingest changed cache: %+v", cached)
	}
	const practicalAllocationCeiling = 32 << 20
	if allocated > practicalAllocationCeiling {
		t.Fatalf("malformed allocation=%d ceiling=%d", allocated, practicalAllocationCeiling)
	}
	t.Logf("full handleIngest malformed envelope: body=%d allocated=%d ceiling=%d", len(malformed), allocated, practicalAllocationCeiling)
}

func TestNearLimitExactIngestAllocationEvidence(t *testing.T) {
	t.Setenv("TEST_WRITER_TOKEN", "worker-secret")
	service := New()
	if err := service.Configure(validConfigYAML()); err != nil {
		t.Fatal(err)
	}
	acceptedCatalog := append([]byte(`{"models":[]}`), bytes.Repeat([]byte(" "), maxCatalogBytes-len(`{"models":[]}`))...)
	acceptedBody := ingestBody(acceptedCatalog)
	rejectedBody := append([]byte(`{"catalog_base64":"`), bytes.Repeat([]byte("A"), maxIngestBodyBytes-len(`{"catalog_base64":"`)-2)...)
	rejectedBody = append(rejectedBody, '"', '}')

	acceptedAllocated, accepted := measureIngestAllocation(service, acceptedBody)
	if accepted.StatusCode != http.StatusOK {
		t.Fatalf("near-limit exact accepted=%d %s", accepted.StatusCode, accepted.Body)
	}
	rejectedAllocated, rejected := measureIngestAllocation(service, rejectedBody)
	if rejected.StatusCode != http.StatusBadRequest || string(rejected.Body) != `{"error_code":"catalog_invalid"}` {
		t.Fatalf("near-limit exact rejected=%d %s", rejected.StatusCode, rejected.Body)
	}
	const normalAllocationCeiling = 128 << 20
	if acceptedAllocated > normalAllocationCeiling || rejectedAllocated > normalAllocationCeiling {
		t.Fatalf("near-limit allocation: accepted=%d rejected=%d ceiling=%d", acceptedAllocated, rejectedAllocated, normalAllocationCeiling)
	}
	t.Logf("full handleIngest near-limit exact: accepted body=%d allocated=%d; rejected body=%d allocated=%d; ceiling=%d", len(acceptedBody), acceptedAllocated, len(rejectedBody), rejectedAllocated, normalAllocationCeiling)
}

func TestFailedIngestPreservesCacheAndSanitizesErrors(t *testing.T) {
	t.Setenv("TEST_WRITER_TOKEN", "worker-secret")
	service := New()
	if err := service.Configure(validConfigYAML()); err != nil {
		t.Fatal(err)
	}
	baseline := []byte(`{"models":[{"slug":"stable","context_window":1}]}`)
	if response := service.HandleManagement(request(http.MethodPost, ingestPath, "worker-secret", ingestBody(baseline))); response.StatusCode != http.StatusOK {
		t.Fatal(string(response.Body))
	}
	deep := strings.Repeat(`{"x":`, maxCatalogJSONDepth+2) + `0` + strings.Repeat(`}`, maxCatalogJSONDepth+2)
	mixed := `{"models":[{"slug":"valid"},{"slug":"invalid","max_tokens":-1}]}`
	for name, body := range map[string][]byte{
		"bad envelope":       []byte(`{"catalog_base64":"e30=","extra":true}`),
		"folded envelope":    []byte(`{"CATALOG_BASE64":"e30="}`),
		"duplicate envelope": []byte(`{"catalog_base64":"e30=","catalog_base64":"e30="}`),
		"trailing envelope":  []byte(`{"catalog_base64":"e30="} {}`),
		"bad base64":         []byte(`{"catalog_base64":"***"}`),
		"malformed":          ingestBody([]byte(`{"models":[`)),
		"folded catalog":     ingestBody([]byte(`{"MODELS":[]}`)),
		"duplicate catalog":  ingestBody([]byte(`{"models":[],"Models":[]}`)),
		"trailing catalog":   ingestBody([]byte(`{"models":[]} {}`)),
		"deep catalog":       ingestBody([]byte(`{"models":[{"slug":"deep","extra":` + deep + `}]}`)),
		"mixed":              ingestBody([]byte(mixed)),
		"duplicate ids":      ingestBody([]byte(`{"models":[{"slug":"same"},{"id":"same"}]}`)),
		"ambiguous id":       ingestBody([]byte(`{"models":[{"ID":"bad","slug":"good"}]}`)),
		"different id slug":  ingestBody([]byte(`{"models":[{"id":"one","slug":"two"}]}`)),
		"empty id":           ingestBody([]byte(`{"models":[{"id":" ","slug":"good"}]}`)),
		"control id":         ingestBody([]byte(`{"models":[{"slug":"bad\u0001"}]}`)),
		"numeric string":     ingestBody([]byte(`{"models":[{"slug":"bad","max_tokens":"1"}]}`)),
		"fractional":         ingestBody([]byte(`{"models":[{"slug":"bad","max_tokens":1.5}]}`)),
		"numeric array":      ingestBody([]byte(`{"models":[{"slug":"bad","max_tokens":[]}]}`)),
		"bad modalities":     ingestBody([]byte(`{"models":[{"slug":"bad","input_modalities":"text"}]}`)),
		"bad reasoning":      ingestBody([]byte(`{"models":[{"slug":"bad","supported_reasoning_levels":[{"Effort":"low"}]}]}`)),
		"bad utf8":           ingestBody([]byte{0xff}),
	} {
		t.Run(name, func(t *testing.T) {
			response := service.HandleManagement(request(http.MethodPost, ingestPath, "worker-secret", body))
			if response.StatusCode != http.StatusBadRequest || string(response.Body) != `{"error_code":"catalog_invalid"}` || service.Last().Count != 1 || service.Last().Models[0].ID != "stable" || bytes.Contains(response.Body, body) {
				t.Fatalf("response=%d %s cache=%+v", response.StatusCode, response.Body, service.Last())
			}
		})
	}

	for name, encoded := range map[string]string{
		"cr":               "eyJtb2RlbHMiOltdfQ==\r",
		"lf":               "eyJtb2RlbHMiOltdfQ==\n",
		"space":            "eyJtb2RlbHMi OltdfQ==",
		"url alphabet":     "_w==",
		"missing padding":  "eyJtb2RlbHMiOltdfQ",
		"extra padding":    "eyJtb2RlbHMiOltdfQ===",
		"nonzero pad bits": "Zh==",
	} {
		t.Run("base64 "+name, func(t *testing.T) {
			body, _ := json.Marshal(ingestRequest{CatalogBase64: encoded})
			response := service.HandleManagement(request(http.MethodPost, ingestPath, "worker-secret", body))
			if response.StatusCode != http.StatusBadRequest || string(response.Body) != `{"error_code":"catalog_invalid"}` || service.Last().Count != 1 || service.Last().Models[0].ID != "stable" {
				t.Fatalf("response=%d %s cache=%+v", response.StatusCode, response.Body, service.Last())
			}
		})
	}

	decodedOversize := bytes.Repeat([]byte("x"), maxCatalogBytes+1)
	response := service.HandleManagement(request(http.MethodPost, ingestPath, "worker-secret", ingestBody(decodedOversize)))
	if response.StatusCode != http.StatusRequestEntityTooLarge || string(response.Body) != `{"error_code":"catalog_too_large"}` || service.Last().Count != 1 || service.Last().Models[0].ID != "stable" {
		t.Fatalf("decoded oversize=%d %s", response.StatusCode, response.Body)
	}
	response = service.HandleManagement(request(http.MethodPost, ingestPath, "worker-secret", bytes.Repeat([]byte("x"), maxIngestBodyBytes+1)))
	if response.StatusCode != http.StatusRequestEntityTooLarge || string(response.Body) != `{"error_code":"catalog_too_large"}` || service.Last().Count != 1 || service.Last().Models[0].ID != "stable" {
		t.Fatalf("encoded oversize=%d %s", response.StatusCode, response.Body)
	}
	response = service.HandleManagement(request(http.MethodPost, ingestPath, "wrong", ingestBody([]byte(`{"models":[]}`))))
	if response.StatusCode != http.StatusUnauthorized || string(response.Body) != `{"error_code":"unauthorized"}` || strings.Contains(string(response.Body), "wrong") {
		t.Fatalf("unauthorized=%d %s", response.StatusCode, response.Body)
	}
}

func TestEmptyCatalogAndTokenRotationStaleIngestRace(t *testing.T) {
	t.Setenv("OLD_TOKEN", "old-secret")
	service := New()
	if err := service.Configure([]byte("worker_token_env: OLD_TOKEN\n")); err != nil {
		t.Fatal(err)
	}
	generation, ok := service.authorize("old-secret")
	if !ok {
		t.Fatal("old token not authorized")
	}
	t.Setenv("NEW_TOKEN", "new-secret")
	if err := service.Configure([]byte("worker_token_env: NEW_TOKEN\n")); err != nil {
		t.Fatal(err)
	}
	if service.replaceCatalog(generation, Catalog{Count: 1, Models: []ModelRow{{ID: "stale"}}}) {
		t.Fatal("stale authorized ingest replaced cache")
	}
	if response := service.HandleManagement(request(http.MethodPost, ingestPath, "old-secret", ingestBody([]byte(`{"models":[]}`)))); response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("old token status=%d", response.StatusCode)
	}
	response := service.HandleManagement(request(http.MethodPost, ingestPath, "new-secret", ingestBody([]byte(`{"models":[]}`))))
	if response.StatusCode != http.StatusOK || string(response.Body) == "" || service.Last().Count != 0 || service.Last().Models == nil || len(service.Last().Models) != 0 {
		t.Fatalf("empty ingest=%d %s cache=%+v", response.StatusCode, response.Body, service.Last())
	}
	for _, path := range []string{catalogPath, lastPath, effectivePath} {
		view := service.HandleManagement(request(http.MethodGet, path, "", nil))
		if !bytes.Contains(view.Body, []byte(`"models":[]`)) || bytes.Contains(view.Body, []byte(`"models":null`)) {
			t.Fatalf("empty view %s=%s", path, view.Body)
		}
	}
}

func TestConcurrentIngestAndReconfigureRejectsStaleGeneration(t *testing.T) {
	t.Setenv("OLD_TOKEN", "old-secret")
	t.Setenv("NEW_TOKEN", "new-secret")
	service := New()
	if err := service.Configure([]byte("worker_token_env: OLD_TOKEN\n")); err != nil {
		t.Fatal(err)
	}
	generation, ok := service.authorize("old-secret")
	if !ok {
		t.Fatal("not authorized")
	}
	var wait sync.WaitGroup
	wait.Add(1)
	go func() {
		defer wait.Done()
		_ = service.Configure([]byte("worker_token_env: NEW_TOKEN\n"))
	}()
	wait.Wait()
	if service.replaceCatalog(generation, Catalog{Count: 1, Models: []ModelRow{{ID: "stale"}}}) {
		t.Fatal("stale generation committed")
	}
}

func TestSameConfigIngestOrderingAndSequentialRefresh(t *testing.T) {
	t.Setenv("TEST_WRITER_TOKEN", "worker-secret")
	service := New()
	if err := service.Configure(validConfigYAML()); err != nil {
		t.Fatal(err)
	}
	older, ok := service.authorize("worker-secret")
	if !ok {
		t.Fatal("older ingest not authorized")
	}
	newer, ok := service.authorize("worker-secret")
	if !ok {
		t.Fatal("newer ingest not authorized")
	}
	if !service.replaceCatalog(newer, Catalog{Count: 1, Models: []ModelRow{{ID: "newer"}}}) {
		t.Fatal("newer ingest did not commit")
	}
	if service.replaceCatalog(older, Catalog{Count: 1, Models: []ModelRow{{ID: "older"}}}) || service.Last().Models[0].ID != "newer" {
		t.Fatalf("older ingest replaced newer cache: %+v", service.Last())
	}
	sequential, ok := service.authorize("worker-secret")
	if !ok || !service.replaceCatalog(sequential, Catalog{Count: 1, Models: []ModelRow{{ID: "sequential"}}}) || service.Last().Models[0].ID != "sequential" {
		t.Fatalf("sequential refresh failed: %+v", service.Last())
	}
}

func TestResourceUsesExactCorePathAndUnknownAPIRoutes404(t *testing.T) {
	service := New()
	resource := service.HandleManagement(request(http.MethodGet, resourceIndexPath, "", nil))
	if resource.StatusCode != http.StatusOK || resource.Headers.Get("Content-Type") != "text/html; charset=utf-8" || !bytes.Equal(resource.Body, uiHTML) {
		t.Fatalf("resource=%d %q %d", resource.StatusCode, resource.Headers.Get("Content-Type"), len(resource.Body))
	}
	for _, path := range []string{"/index.html", "/v0/resource/plugins/model-info/unknown.html", "/v0/management/plugins/model-info/unknown"} {
		response := service.HandleManagement(request(http.MethodGet, path, "", nil))
		if response.StatusCode != http.StatusNotFound || string(response.Body) != `{"error_code":"not_found"}` {
			t.Fatalf("unknown %s=%d %s", path, response.StatusCode, response.Body)
		}
	}
}

func TestExactCoreRichCatalogFixture(t *testing.T) {
	raw, err := os.ReadFile("testdata/core_codex_client_models.json")
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := parseCatalog(raw)
	if err != nil {
		t.Fatal(err)
	}
	if catalog.Count == 0 || catalog.Models[0].ID == "" || catalog.Models[0].Context <= 0 || len(catalog.Models[0].Levels) == 0 || len(catalog.Models[0].Input) == 0 {
		t.Fatalf("catalog=%+v", catalog)
	}
}

func TestUIUsesWriterAndEscapesCatalogHTML(t *testing.T) {
	html := string(uiHTML)
	for _, want := range []string{
		"/v0/management/plugins/sync-config-write",
		"fetch(writerAPI + '/model-info/catalog', {method: 'POST', headers: headers()})",
		"/status?run_id=",
		"fetch(modelInfoAPI + '/catalog'",
		"function esc(value)",
		"${esc(m.id)}",
		"replaceChildren(new Option",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("UI missing %q", want)
		}
	}
	for _, forbidden := range []string{"modelInfoAPI + '/catalog', {method: 'POST'", "worker-secret", "X-Sync-Config-Writer-Token"} {
		if strings.Contains(html, forbidden) {
			t.Fatalf("UI contains forbidden %q", forbidden)
		}
	}
}
