package plugin

import "testing"

func TestSelectedSnapshotRejectsCredentialAndIndirectHeaders(t *testing.T) {
	spec := compiledChannel{Enabled: true, Kind: KindOpenAI, Selector: ChannelSelector{Name: "provider", BaseURL: "https://provider.example/v1"}}
	cases := map[string]string{
		"authorization": `headers: {Authorization: "Bearer secret"}`,
		"api key":       `headers: {x-api-key: secret}`,
		"host":          `headers: {Host: evil.example}`,
		"worker token":  `headers: {X-Sync-Config-Writer-Token: secret}`,
		"non scalar":    `headers: {Accept: [application/json]}`,
		"alias":         `headers: &headers {Accept: application/json}`,
	}
	for name, headers := range cases {
		t.Run(name, func(t *testing.T) {
			raw := "openai-compatibility:\n  - name: provider\n    base-url: https://provider.example/v1\n    " + headers + "\n    models: [{name: a}]\n"
			document, err := parseSnapshot([]byte(raw))
			if err == nil {
				_, err = locateSnapshotChannel(document, spec)
			}
			if err == nil {
				t.Fatal("unsafe or indirect headers accepted")
			}
		})
	}
}

func TestSelectedSnapshotRejectsAmbiguousOrMalformedModels(t *testing.T) {
	spec := compiledChannel{Enabled: true, Kind: KindOpenAI, Selector: ChannelSelector{Name: "provider", BaseURL: "https://provider.example/v1"}}
	cases := map[string]string{
		"duplicate model": `openai-compatibility:
  - name: provider
    base-url: https://provider.example/v1
    models: [{name: a}, {name: a}]
`,
		"model alias": `openai-compatibility:
  - name: provider
    base-url: https://provider.example/v1
    models:
      - &shared {name: a}
      - *shared
`,
		"model merge": `openai-compatibility:
  - name: provider
    base-url: https://provider.example/v1
    models:
      - &base {name: a}
      - {<<: *base, name: b}
`,
		"string limit": `openai-compatibility:
  - name: provider
    base-url: https://provider.example/v1
    models: [{name: a, max-output-tokens: "4"}]
`,
		"zero limit": `openai-compatibility:
  - name: provider
    base-url: https://provider.example/v1
    models: [{name: a, max-output-tokens: 0}]
`,
		"thinking scalar": `openai-compatibility:
  - name: provider
    base-url: https://provider.example/v1
    models: [{name: a, thinking: enabled}]
`,
		"modalities scalar": `openai-compatibility:
  - name: provider
    base-url: https://provider.example/v1
    models: [{name: a, input-modalities: text}]
`,
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			document, err := parseSnapshot([]byte(raw))
			if err != nil {
				return
			}
			channel, err := locateSnapshotChannel(document, spec)
			if err == nil {
				_, err = snapshotModels(channel)
			}
			if err == nil {
				t.Fatal("ambiguous or malformed selected model accepted")
			}
		})
	}
}
