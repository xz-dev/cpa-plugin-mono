package plugin

import "testing"

func TestParseMembershipCatalogShapes(t *testing.T) {
	for name, raw := range map[string]string{
		"data":     `{"data":[{"id":"a"},{"name":"b"}]}`,
		"manifest": `{"models":[{"slug":"a"},{"id":"b"}]}`,
		"array":    `["a",{"name":"b"}]`,
	} {
		t.Run(name, func(t *testing.T) {
			ids, err := parseUpstreamModels([]byte(raw))
			if err != nil {
				t.Fatal(err)
			}
			if len(ids) != 2 || ids[0] != "a" || ids[1] != "b" {
				t.Fatalf("ids=%v", ids)
			}
		})
	}
}
