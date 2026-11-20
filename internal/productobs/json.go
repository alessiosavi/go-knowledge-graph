package productobs

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strings"
)

type object map[string]jsontext.Value

// decodeJSON uses the strict decoder defaults, which reject invalid UTF-8 and
// duplicate names at every depth. Decoder diagnostics may contain input data.
func decodeJSON(data []byte, dest any) error {
	err := json.Unmarshal(data, dest)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, jsontext.ErrDuplicateName):
		return errors.New("duplicate JSON member")
	default:
		return errors.New("invalid JSON")
	}
}

func readObject(data []byte, fields string) (object, error) {
	var obj object
	if err := decodeJSON(data, &obj); err != nil {
		return nil, err
	}
	if obj == nil {
		return nil, errors.New("expected JSON object")
	}
	if fields != "" {
		names := strings.Fields(fields)
		if len(obj) != len(names) {
			return nil, errors.New("missing or unexpected fields")
		}
		for _, name := range names {
			if obj[name] == nil {
				return nil, errors.New("missing or unexpected fields")
			}
		}
	}
	return obj, nil
}

func readArray(value jsontext.Value) ([]jsontext.Value, error) {
	if value.Kind() != jsontext.KindBeginArray {
		return nil, errors.New("expected JSON array")
	}
	var items []jsontext.Value
	if err := decodeJSON(value, &items); err != nil {
		return nil, err
	}
	return items, nil
}

func readString(value jsontext.Value) (string, bool) {
	if value.Kind() != jsontext.KindString {
		return "", false
	}
	var s string
	err := decodeJSON(value, &s)
	return s, err == nil
}

func uniqueStrings(value jsontext.Value) (map[string]struct{}, error) {
	items, err := readArray(value)
	if err != nil {
		return nil, err
	}
	result := make(map[string]struct{}, len(items))
	for _, item := range items {
		s, ok := readString(item)
		if !ok || !nonempty(s) {
			return nil, errors.New("expected unique non-empty strings")
		}
		if _, seen := result[s]; seen {
			return nil, errors.New("expected unique non-empty strings")
		}
		result[s] = struct{}{}
	}
	return result, nil
}

func fieldError(field string) error {
	return fmt.Errorf("%s: expected string", field)
}
