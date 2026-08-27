package plugin

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"
)

const maxCatalogJSONDepth = 128

func validateCatalogJSON(raw []byte) error {
	if !utf8.Valid(raw) || len(bytes.TrimSpace(raw)) == 0 {
		return fmt.Errorf("invalid catalog JSON")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := scanCatalogJSON(decoder, 0); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return fmt.Errorf("trailing catalog JSON")
	}
	return nil
}

func scanCatalogJSON(decoder *json.Decoder, depth int) error {
	if depth > maxCatalogJSONDepth {
		return fmt.Errorf("catalog JSON is too deeply nested")
	}
	token, err := decoder.Token()
	if err != nil {
		return fmt.Errorf("invalid catalog JSON")
	}
	delim, composite := token.(json.Delim)
	if !composite {
		return nil
	}
	switch delim {
	case '{':
		seen := make(map[string]bool)
		for decoder.More() {
			keyToken, err := decoder.Token()
			key, ok := keyToken.(string)
			folded := strings.ToUpper(key)
			if err != nil || !ok || seen[folded] {
				return fmt.Errorf("invalid catalog object key")
			}
			seen[folded] = true
			if err := scanCatalogJSON(decoder, depth+1); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim('}') {
			return fmt.Errorf("invalid catalog object")
		}
	case '[':
		for decoder.More() {
			if err := scanCatalogJSON(decoder, depth+1); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim(']') {
			return fmt.Errorf("invalid catalog array")
		}
	default:
		return fmt.Errorf("invalid catalog JSON")
	}
	return nil
}

func catalogObject(raw []byte) (map[string]json.RawMessage, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return nil, fmt.Errorf("invalid catalog object")
	}
	return fields, nil
}

func hasAnyFoldedCatalogField(fields map[string]json.RawMessage, canonical ...string) bool {
	for _, name := range canonical {
		if hasFoldedCatalogField(fields, name) {
			return true
		}
	}
	return false
}

func hasFoldedCatalogField(fields map[string]json.RawMessage, canonical string) bool {
	for key := range fields {
		if strings.EqualFold(key, canonical) && key != canonical {
			return true
		}
	}
	return false
}
