package convert

import (
	"bytes"
	"context"
	"errors"
	"strings"
)

// WithoutImages prevents images from reaching the wrapped converter.
func WithoutImages(c Converter) Converter {
	return withoutImages{inner: c}
}

type withoutImages struct {
	inner Converter
}

func (c withoutImages) Convert(ctx context.Context, body []byte, filename, contentType string) (string, error) {
	ct := strings.ToLower(strings.TrimSpace(strings.SplitN(contentType, ";", 2)[0]))
	// Mirror ingest/load.go's image signatures, including its BOM tolerance.
	// ZIP's signature belongs to Office documents and must pass through.
	b := bytes.TrimPrefix(body, []byte{0xEF, 0xBB, 0xBF})
	if strings.HasPrefix(ct, "image/") ||
		bytes.HasPrefix(b, []byte{0x89, 'P', 'N', 'G'}) ||
		bytes.HasPrefix(b, []byte{0xFF, 0xD8, 0xFF}) ||
		bytes.HasPrefix(b, []byte{'I', 'I', 0x2A, 0x00}) ||
		bytes.HasPrefix(b, []byte{'M', 'M', 0x00, 0x2A}) ||
		(len(b) >= 12 && bytes.HasPrefix(b, []byte("RIFF")) && bytes.Equal(b[8:12], []byte("WEBP"))) ||
		bytes.HasPrefix(b, []byte("BM")) ||
		bytes.HasPrefix(b, []byte("GIF87a")) ||
		bytes.HasPrefix(b, []byte("GIF89a")) {
		return "", errors.New("image conversion is disabled (set GKG_CONVERTER_IMAGES=true to send images to docling; docling-serve v1.1.0 on CPU runs out of memory on images)")
	}
	return c.inner.Convert(ctx, body, filename, contentType)
}
