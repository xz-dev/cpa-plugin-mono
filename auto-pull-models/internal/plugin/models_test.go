package plugin

import (
	"strings"
	"testing"
)

func TestParseMembershipCatalogShapes(t *testing.T) {
	for name, raw := range map[string]string{
		"data":             `{"data":[{"id":"a"},{"name":"b"}],"object":"list"}`,
		"manifest":         `{"models":[{"slug":"a"},{"id":"b"}]}`,
		"array":            `["a",{"name":"b"}]`,
		"same identifiers": `{"data":[{"id":"a","slug":"a","name":"a"}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			ids, err := parseUpstreamModels([]byte(raw))
			if err != nil {
				t.Fatal(err)
			}
			want := 2
			if name == "same identifiers" {
				want = 1
			}
			if len(ids) != want || ids[0] != "a" || want == 2 && ids[1] != "b" {
				t.Fatalf("ids=%v", ids)
			}
		})
	}
	if _, err := parseUpstreamModels([]byte(`{"data":[{"id":"duplicate"},{"id":"duplicate"}]}`)); err == nil {
		t.Fatal("duplicate upstream model IDs accepted")
	}
}

func TestParseMembershipCatalogFailsClosedOnAmbiguity(t *testing.T) {
	for name, raw := range map[string]string{
		"mixed valid invalid": `{"data":[{"id":"kept"},{"unexpected":"secret-body-marker"}]}`,
		"both wrappers":       `{"data":[{"id":"a"}],"models":[{"id":"b"}]}`,
		"duplicate wrapper":   `{"data":[{"id":"a"}],"data":[{"id":"b"}]}`,
		"folded wrapper":      `{"DATA":[{"id":"a"}]}`,
		"null wrapper":        `{"data":null}`,
		"wrong wrapper type":  `{"data":{"id":"a"}}`,
		"duplicate id":        `{"data":[{"id":"a","id":"b"}]}`,
		"folded id":           `{"data":[{"ID":"a"}]}`,
		"conflicting ids":     `{"data":[{"id":"a","name":"b"}]}`,
		"empty id":            `{"data":[{"id":" "}]}`,
		"non-string id":       `{"data":[{"id":1}]}`,
		"non-object entry":    `{"data":[1]}`,
		"trailing payload":    `{"data":[{"id":"a"}]} {}`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := parseUpstreamModels([]byte(raw))
			if err == nil {
				t.Fatal("ambiguous catalog accepted")
			}
			if strings.Contains(err.Error(), "secret-body-marker") {
				t.Fatalf("catalog body leaked: %v", err)
			}
		})
	}
}
