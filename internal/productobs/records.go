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
	return parseRecordWithCaps(raw, false)
}

// ParseImportRecord decodes one strict A1 record with the snapshot row caps.
func ParseImportRecord(raw []byte) (Record, error) {
	if len(raw) > MaxImportRowBytes {
		return Record{}, errors.New("row exceeds 64 KiB")
	}
	return parseRecordWithCaps(raw, true)
}

// ImportIdentityPrefix validates the snapshot import identity's store prefix.
func ImportIdentityPrefix(productID string) (string, error) {
	store, _, found := strings.Cut(productID, ":")
	if !found || !storePattern.MatchString(store) {
		return "", errors.New("product_id must have a valid store prefix")
	}
	return store, nil
}

// ProductStoreFromID validates the shared write identity and returns its store.
func ProductStoreFromID(id string) (string, error) {
	store, suffix, found := strings.Cut(id, ":")
	if len(id) > MaxImportIdentityBytes || !found || suffix == "" || !storePattern.MatchString(store) {
		return "", errors.New("product_id must have a valid store prefix and a non-empty suffix, up to 1 KiB")
	}
	return store, nil
}

func parseRecordWithCaps(raw []byte, capped bool) (Record, error) {
	var record Record
	obj, err := readObject(raw, "store product_id title brand category subcategory product_type link attributes page_specs")
	if err != nil {
		return record, err
	}
	for _, field := range []struct {
		name     string
		dest     *string
		maxBytes int
	}{
		{"store", &record.Store, MaxImportIdentityBytes},
		{"product_id", &record.ProductID, MaxImportIdentityBytes},
		{"title", &record.Title, MaxImportProductScalarBytes},
		{"brand", &record.Brand, MaxImportProductScalarBytes},
		{"category", &record.Category, MaxImportProductScalarBytes},
		{"subcategory", &record.Subcategory, MaxImportProductScalarBytes},
		{"link", &record.Link, MaxImportProductScalarBytes},
	} {
		value, ok := readString(obj[field.name])
		if !ok {
			return record, fieldError(field.name)
		}
		if capped && len(value) > field.maxBytes {
			return record, fmt.Errorf("%s value exceeds %d KiB", field.name, field.maxBytes>>10)
		}
		*field.dest = value
	}
	prefix, err := ImportIdentityPrefix(record.ProductID)
	if err != nil || prefix != record.Store {
		return record, errors.New("store must match the valid store prefix of product_id")
	}
	if obj["product_type"].Kind() != jsontext.KindNull {
		productType, ok := readString(obj["product_type"])
		if !ok {
			return record, errors.New("product_type: expected string or null")
		}
		if capped && len(productType) > MaxImportProductScalarBytes {
			return record, errors.New("product_type value exceeds 4 KiB")
		}
		record.ProductType = &productType
	}

	attributes, err := readObject(obj["attributes"], "")
	if err != nil {
		return record, fmt.Errorf("attributes: %w", err)
	}
	if capped && len(attributes) > MaxImportAttributes {
		return record, errors.New("row exceeds 512 attributes")
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
