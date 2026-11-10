package convert_test

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/alessiosavi/go-knowledge-graph/internal/convert"
)

type imageTestConverter func(context.Context, []byte, string, string) (string, error)

func (f imageTestConverter) Convert(ctx context.Context, body []byte, filename, contentType string) (string, error) {
	return f(ctx, body, filename, contentType)
}

func TestWithoutImages(t *testing.T) {
	for _, tc := range []struct {
		name, body, contentType string
		blocked                 bool
	}{
		{"PNG body", "\x89PNG\r\n\x1a\n", "application/octet-stream", true},
		{"JPEG content type", "opaque", "image/jpeg", true},
		{"GIF89a magic", "GIF89a", "", true},
		{"GIF87a magic", "GIF87a", "", true},
		{"JPEG magic", "\xff\xd8\xff", "", true},
		{"TIFF little endian", "II\x2a\x00", "", true},
		{"TIFF big endian", "MM\x00\x2a", "", true},
		{"WEBP magic", "RIFF\x00\x00\x00\x00WEBP", "", true},
		{"BMP magic", "BM", "", true},
		{"BOM PNG", "\xef\xbb\xbf\x89PNG", "", true},
		{"image type parameters", "opaque", " Image/SVG+XML; charset=utf-8 ", true},
		{"PDF", "%PDF-1.7", "application/pdf", false},
		{"DOCX", "PK\x03\x04", "application/vnd.openxmlformats-officedocument.wordprocessingml.document", false},
		{"short RIFF", "RIFF", "", false},
		{"empty", "", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(tc.body)
			ctx := context.WithValue(context.Background(), struct{}{}, "marker")
			calls := 0
			wantErr := errors.New("inner failure")
			inner := imageTestConverter(func(gotCtx context.Context, gotBody []byte, filename, contentType string) (string, error) {
				calls++
				if gotCtx != ctx || !bytes.Equal(gotBody, body) || filename != "document" || contentType != tc.contentType {
					t.Error("guard changed converter arguments")
				}
				return "markdown", wantErr
			})
			md, err := convert.WithoutImages(inner).Convert(ctx, body, "document", tc.contentType)
			if tc.blocked {
				const want = "image conversion is disabled (set GKG_CONVERTER_IMAGES=true to send images to docling; docling-serve v1.1.0 on CPU runs out of memory on images)"
				if err == nil || !strings.Contains(err.Error(), want) || md != "" || calls != 0 {
					t.Fatalf("image must fail with GKG_CONVERTER_IMAGES guidance before inner call: calls=%d, err=%v", calls, err)
				}
			} else if calls != 1 || md != "markdown" || err != wantErr {
				t.Fatalf("non-image result must pass through unchanged: calls=%d, err=%v", calls, err)
			}
		})
	}
}
