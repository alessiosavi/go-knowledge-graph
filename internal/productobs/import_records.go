package productobs

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
)

// MaxImportProducts bounds the number of rows admitted to a store snapshot.
const MaxImportProducts = 10000

const (
	MaxImportRowBytes           = 64 << 10
	MaxImportIdentityBytes      = 1 << 10
	MaxImportProductScalarBytes = 4 << 10
	MaxImportAttributes         = 512
	MaxImportValueBytes         = 1 << 10
	MaxImportItems              = 64
)

// ScanImportRecords validates the input envelope in bounded space, hashes its
// original bytes, and calls visit once per valid row. The caller applies limits
// on released observations after projection and decides how to report rejections.
// The scan stops at the first row past the product cap or a visitor error.
func ScanImportRecords(ctx context.Context, source io.Reader, store string, reject func(int, error), visit func(int, Record) error) (string, error) {
	hash := sha256.New()
	reader := bufio.NewReaderSize(io.TeeReader(source, hash), MaxImportRowBytes+1)
	seen := make(map[string]struct{})
	for line := 1; ; line++ {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		raw, err := reader.ReadSlice('\n')
		if len(raw) == 0 && errors.Is(err, io.EOF) {
			break
		}
		if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, bufio.ErrBufferFull) {
			return "", fmt.Errorf("read records line %d: %w", line, err)
		}
		if line > MaxImportProducts {
			reject(line, errors.New("snapshot exceeds 10,000 products"))
			return "", nil
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			reject(line, errors.New("row exceeds 64 KiB"))
			for errors.Is(err, bufio.ErrBufferFull) {
				if err := ctx.Err(); err != nil {
					return "", err
				}
				_, err = reader.ReadSlice('\n')
			}
			if err != nil && !errors.Is(err, io.EOF) {
				return "", fmt.Errorf("read records line %d: %w", line, err)
			}
			continue
		}
		record, recordErr := ParseImportRecord(bytes.TrimSuffix(raw, []byte{'\n'}))
		if recordErr == nil && record.Store != store {
			recordErr = errors.New("row store does not match -store")
		}
		if _, duplicate := seen[record.ProductID]; recordErr == nil && duplicate {
			recordErr = errors.New("duplicate product_id")
		}
		if recordErr != nil {
			reject(line, recordErr)
			continue
		}
		seen[record.ProductID] = struct{}{}
		if visit != nil {
			if err := visit(line, record); err != nil {
				return "", err
			}
		}
	}
	return fmt.Sprintf("%x", hash.Sum(nil)), nil
}
