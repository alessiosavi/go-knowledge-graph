package productobs

import "errors"

// MaxPushEventBytes bounds the JSON envelope around one A1 record.
const MaxPushEventBytes = 128 << 10

// ParsePushEvent decodes the strict envelope. The caller validates and projects
// the A1 record against the active release before writing it.
func ParsePushEvent(raw []byte) (int64, []byte, error) {
	if len(raw) > MaxPushEventBytes {
		return 0, nil, errors.New("push event exceeds 128 KiB")
	}
	fields, err := readObject(raw, "seq record")
	if err != nil {
		return 0, nil, err
	}
	var seq int64
	if err := decodeJSON(fields["seq"], &seq); err != nil || seq < 1 {
		return 0, nil, errors.New("seq must be a positive int64")
	}
	return seq, fields["record"], nil
}
