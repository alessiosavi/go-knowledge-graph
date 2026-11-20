package productobs

import (
	"bufio"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"io"
	"strings"
)

// Record is an A1 product envelope. Page specs are validated on input and
// discarded. The emitter derives observations from attributes.
type Record struct {
	ProductID, Store, Title, Brand, Category, Subcategory, Link string
	ProductType                                                 *string
	Attributes                                                  map[string]string
}

// ReadRecords validates A1 JSONL records and rejects duplicate product IDs.
// It buffers the entire input as decoded records without enforcing import caps.
// Apply the import caps before using this reader on an import path.
func ReadRecords(source io.Reader) ([]Record, error) {
	reader := bufio.NewReader(source)
	seen := make(map[string]struct{})
	var records []Record
	for lineNumber := 1; ; lineNumber++ {
		line, readErr := reader.ReadBytes('\n')
		if len(line) != 0 {
			record, err := parseRecord(line)
			if err != nil {
				return nil, fmt.Errorf("records line %d: %w", lineNumber, err)
			}
			if _, exists := seen[record.ProductID]; exists {
				return nil, fmt.Errorf("records line %d: duplicate product_id", lineNumber)
			}
			seen[record.ProductID] = struct{}{}
			records = append(records, record)
		}
		if errors.Is(readErr, io.EOF) {
			return records, nil
		}
		if readErr != nil {
			return nil, fmt.Errorf("read records line %d: %w", lineNumber, readErr)
		}
	}
}

func parseRecord(raw []byte) (Record, error) {
	var record Record
	obj, err := readObject(raw, "store product_id title brand category subcategory product_type link attributes page_specs")
	if err != nil {
		return record, err
	}
	for _, field := range []struct {
		name string
		dest *string
	}{
		{"store", &record.Store}, {"product_id", &record.ProductID}, {"title", &record.Title},
		{"brand", &record.Brand}, {"category", &record.Category}, {"subcategory", &record.Subcategory},
		{"link", &record.Link},
	} {
		value, ok := readString(obj[field.name])
		if !ok {
			return record, fieldError(field.name)
		}
		*field.dest = value
	}
	prefix, _, found := strings.Cut(record.ProductID, ":")
	if !found || prefix != record.Store || !storePattern.MatchString(record.Store) {
		return record, errors.New("store must match the valid store prefix of product_id")
	}
	if obj["product_type"].Kind() != jsontext.KindNull {
		productType, ok := readString(obj["product_type"])
		if !ok {
			return record, errors.New("product_type: expected string or null")
		}
		record.ProductType = &productType
	}

	attributes, err := readObject(obj["attributes"], "")
	if err != nil {
		return record, fmt.Errorf("attributes: %w", err)
	}
	record.Attributes = make(map[string]string, len(attributes))
	for key, raw := range attributes {
		value, ok := readString(raw)
		if !ok {
			return record, errors.New("attribute values must be strings")
		}
		record.Attributes[key] = value
	}
	if err := validatePageSpecs(obj["page_specs"]); err != nil {
		return record, fmt.Errorf("page_specs: %w", err)
	}
	return record, nil
}

func validatePageSpecs(raw jsontext.Value) error {
	if raw.Kind() == jsontext.KindNull {
		return nil
	}
	pairs, err := readArray(raw)
	if err != nil {
		return err
	}
	for _, pair := range pairs {
		fields, err := readObject(pair, "label value")
		if err != nil {
			return err
		}
		for _, field := range []string{"label", "value"} {
			if fields[field].Kind() != jsontext.KindString {
				return fieldError(field)
			}
		}
	}
	return nil
}
