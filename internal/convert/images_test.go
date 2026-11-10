package convert

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"strings"
	"testing"
	"time"
)

type imageTestConverter func(context.Context, []byte, string, string) (string, error)

func (f imageTestConverter) Convert(ctx context.Context, body []byte, filename, contentType string) (string, error) {
	return f(ctx, body, filename, contentType)
}

func TestImagePolicyDisabled(t *testing.T) {
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
		{"HEIC magic", "\x00\x00\x00\x18ftypheic" + strings.Repeat("\x00", 12), "application/octet-stream", true},
		{"AVIF magic", "\x00\x00\x00\x1cftypavif" + strings.Repeat("\x00", 16), "application/octet-stream", true},
		{"AVIF sequence magic", "\x00\x00\x00\x18ftypavis", "application/octet-stream", true},
		{"HEIX magic", "\x00\x00\x00\x18ftypheix", "application/octet-stream", true},
		{"HEIM magic", "\x00\x00\x00\x18ftypheim", "application/octet-stream", true},
		{"HEIS magic", "\x00\x00\x00\x18ftypheis", "application/octet-stream", true},
		{"HEVC magic", "\x00\x00\x00\x18ftyphevc", "application/octet-stream", true},
		{"HEVX magic", "\x00\x00\x00\x18ftyphevx", "application/octet-stream", true},
		{"HEVM magic", "\x00\x00\x00\x18ftyphevm", "application/octet-stream", true},
		{"HEVS magic", "\x00\x00\x00\x18ftyphevs", "application/octet-stream", true},
		{"HEIF magic", "\x00\x00\x00\x18ftypmif1", "application/octet-stream", true},
		{"HEIF sequence magic", "\x00\x00\x00\x18ftypmsf1", "application/octet-stream", true},
		{"BOM HEIC", "\xef\xbb\xbf\x00\x00\x00\x18ftypheic", "application/octet-stream", true},
		{"BMP magic", "BM", "", true},
		{"BOM PNG", "\xef\xbb\xbf\x89PNG", "", true},
		{"image type parameters", "opaque", " Image/SVG+XML; charset=utf-8 ", true},
		{"PDF", "%PDF-1.7", "application/pdf", false},
		{"DOCX", "PK\x03\x04", "application/vnd.openxmlformats-officedocument.wordprocessingml.document", false},
		{"MP4", "\x00\x00\x00\x18ftypisom" + strings.Repeat("\x00", 12), "application/octet-stream", false},
		{"MP41", "\x00\x00\x00\x18ftypmp41", "application/octet-stream", false},
		{"MP42", "\x00\x00\x00\x18ftypmp42", "application/octet-stream", false},
		{"QuickTime", "\x00\x00\x00\x18ftypqt  ", "application/octet-stream", false},
		{"missing ftyp", "\x00\x00\x00\x18freeheic", "application/octet-stream", false},
		{"short ftyp", "\x00\x00\x00\x18ftyphei", "application/octet-stream", false},
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
			md, err := ImagePolicy(inner, false, 12_500_000).Convert(ctx, body, "document", tc.contentType)
			if tc.blocked {
				const want = "image conversion is disabled (set GKG_CONVERTER_IMAGES=true to send images to docling)"
				if err == nil || !strings.Contains(err.Error(), want) || md != "" || calls != 0 {
					t.Fatalf("image must fail with GKG_CONVERTER_IMAGES guidance before inner call: calls=%d, err=%v", calls, err)
				}
			} else if calls != 1 || md != "markdown" || err != wantErr {
				t.Fatalf("non-image result must pass through unchanged: calls=%d, err=%v", calls, err)
			}
		})
	}
}

// Hand-built headers deliberately have no pixel buffers: the policy must
// reject oversized declarations before any decoder can allocate pixels.
func imageHeader(format string, w, h int) []byte {
	le := binary.LittleEndian
	switch format {
	case "PNG":
		b := make([]byte, 33)
		copy(b, "\x89PNG\r\n\x1a\n\x00\x00\x00\x0dIHDR")
		binary.BigEndian.PutUint32(b[16:], uint32(w))
		binary.BigEndian.PutUint32(b[20:], uint32(h))
		b[24], b[25] = 8, 2
		binary.BigEndian.PutUint32(b[29:], crc32.ChecksumIEEE(b[12:29]))
		return b
	case "JPEG":
		return []byte{255, 216, 255, 192, 0, 17, 8, byte(h >> 8), byte(h), byte(w >> 8), byte(w), 3, 1, 0x11, 0, 2, 0x11, 0, 3, 0x11, 0, 255, 218, 0, 2}
	case "GIF":
		b := make([]byte, 13)
		copy(b, "GIF89a")
		le.PutUint16(b[6:], uint16(w))
		le.PutUint16(b[8:], uint16(h))
		return b
	case "BMP", "BMP top down":
		b := make([]byte, 54)
		copy(b, "BM")
		le.PutUint32(b[14:], 40)
		le.PutUint32(b[18:], uint32(w))
		if format == "BMP top down" {
			h = -h
		}
		le.PutUint32(b[22:], uint32(h))
		le.PutUint16(b[26:], 1)
		le.PutUint16(b[28:], 24)
		return b
	case "BMP core":
		b := make([]byte, 26)
		copy(b, "BM")
		le.PutUint32(b[14:], 12)
		le.PutUint16(b[18:], uint16(w))
		le.PutUint16(b[20:], uint16(h))
		le.PutUint16(b[22:], 1)
		le.PutUint16(b[24:], 24)
		return b
	case "VP8", "VP8L", "VP8X":
		size := 10
		if format == "VP8L" {
			size = 5
		}
		b := make([]byte, 20+size+(size%2))
		copy(b, "RIFF")
		le.PutUint32(b[4:], uint32(len(b)-8))
		copy(b[8:], "WEBP")
		chunk := format
		if format == "VP8" {
			chunk += " "
		}
		copy(b[12:], chunk)
		le.PutUint32(b[16:], uint32(size))
		switch format {
		case "VP8":
			copy(b[23:], []byte{0x9d, 1, 0x2a})
			le.PutUint16(b[26:], uint16(w))
			le.PutUint16(b[28:], uint16(h))
		case "VP8L":
			b[20] = 0x2f
			le.PutUint32(b[21:], uint32(w-1)|uint32(h-1)<<14)
		case "VP8X":
			for i := 0; i < 3; i++ {
				b[24+i] = byte((w - 1) >> (8 * i))
				b[27+i] = byte((h - 1) >> (8 * i))
			}
		}
		return b
	case "TIFF II", "TIFF MM", "TIFF SHORT":
		var order binary.ByteOrder = le
		b := make([]byte, 38)
		copy(b, "II")
		if format == "TIFF MM" {
			order = binary.BigEndian
			copy(b, "MM")
		}
		order.PutUint16(b[2:], 42)
		order.PutUint32(b[4:], 8)
		order.PutUint16(b[8:], 2)
		for i, n := range []int{w, h} {
			e := b[10+12*i:]
			order.PutUint16(e, uint16(256+i))
			order.PutUint16(e[2:], 4)
			order.PutUint32(e[4:], 1)
			order.PutUint32(e[8:], uint32(n))
			if format == "TIFF SHORT" {
				order.PutUint16(e[2:], 3)
				order.PutUint32(e[8:], 0)
				order.PutUint16(e[8:], uint16(n))
			}
		}
		return b
	}
	panic("unknown fixture format")
}

func encodedImage(t *testing.T, format string, transparent bool) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, 2, 3))
	for y := 0; y < 3; y++ {
		for x := 0; x < 2; x++ {
			img.Set(x, y, color.NRGBA{20, 40, 60, 255})
		}
	}
	if transparent {
		img.Set(0, 0, color.NRGBA{0, 0, 0, 0})
		img.Set(1, 0, color.NRGBA{0, 0, 0, 128})
	}
	var b bytes.Buffer
	var err error
	switch format {
	case "PNG":
		err = png.Encode(&b, img)
	case "JPEG":
		err = jpeg.Encode(&b, img, nil)
	case "GIF":
		err = gif.Encode(&b, img, nil)
	}
	if err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

var imageFormats = []string{"PNG", "JPEG", "GIF", "BMP", "BMP top down", "BMP core", "VP8", "VP8L", "VP8X", "TIFF II", "TIFF MM", "TIFF SHORT"}

func TestImagePolicyFormats(t *testing.T) {
	for _, format := range imageFormats {
		t.Run(format, func(t *testing.T) {
			body := imageHeader(format, 2, 3)
			if format == "PNG" || format == "JPEG" || format == "GIF" {
				body = encodedImage(t, format, false)
			}
			calls := 0
			inner := imageTestConverter(func(_ context.Context, got []byte, filename, ct string) (string, error) {
				calls++
				if !bytes.Equal(body, got) || filename != "original" || ct != "application/octet-stream" {
					t.Error("changed image arguments")
				}
				return "converted", nil
			})
			md, err := ImagePolicy(inner, true, 6).Convert(context.Background(), body, "original", "application/octet-stream")
			if err != nil || md != "converted" || calls != 1 {
				t.Fatalf("small image must pass at exact cap: calls=%d err=%v", calls, err)
			}
		})
	}
}

func TestImagePolicyPixelCap(t *testing.T) {
	for _, format := range imageFormats {
		t.Run(format, func(t *testing.T) {
			calls := 0
			inner := imageTestConverter(func(context.Context, []byte, string, string) (string, error) { calls++; return "", nil })
			_, err := ImagePolicy(inner, true, 12_500_000).Convert(context.Background(), imageHeader(format, 16000, 16000), "large", "application/octet-stream")
			if calls != 0 || err == nil {
				t.Fatalf("oversized header must fail before inner: calls=%d err=%v", calls, err)
			}
			for _, want := range []string{"16000", "256", "12.5", "GKG_CONVERTER_MAX_IMAGE_MP", "downscale"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q missing %q", err, want)
				}
			}
		})
	}
	t.Run("PNG decompression bomb", func(t *testing.T) {
		inner := imageTestConverter(func(context.Context, []byte, string, string) (string, error) {
			t.Fatal("bomb reached inner")
			return "", nil
		})
		_, err := ImagePolicy(inner, true, 12_500_000).Convert(context.Background(), imageHeader("PNG", 50000, 50000), "bomb.png", "image/png")
		if err == nil || !strings.Contains(err.Error(), "50000") || !strings.Contains(err.Error(), "GKG_CONVERTER_MAX_IMAGE_MP") {
			t.Fatalf("want header cap rejection, got %v", err)
		}
	})
}

func TestImagePolicyLargeHeaders(t *testing.T) {
	for _, fixture := range []struct {
		name     string
		body     func(t *testing.T, w, h int) []byte
		largeDim int
	}{
		{"TIFF II", func(_ *testing.T, w, h int) []byte {
			return tiffWithLateIFD("TIFF II", w, h)
		}, 16000},
		{"TIFF MM", func(_ *testing.T, w, h int) []byte {
			return tiffWithLateIFD("TIFF MM", w, h)
		}, 16000},
		{"JPEG APP1", func(t *testing.T, w, h int) []byte {
			var encoded bytes.Buffer
			if err := jpeg.Encode(&encoded, image.NewGray(image.Rect(0, 0, w, h)), nil); err != nil {
				t.Fatal(err)
			}
			var body bytes.Buffer
			body.Write(encoded.Bytes()[:2]) // SOI, followed by at least 1.5 MiB of APP1 payloads.
			for remaining := 3 << 19; remaining > 0; {
				size := min(remaining, 65533)
				body.Write([]byte{0xff, 0xe1, byte((size + 2) >> 8), byte(size + 2)})
				body.Write(make([]byte, size))
				remaining -= size
			}
			body.Write(encoded.Bytes()[2:])
			return body.Bytes()
		}, 4},
		{"WEBP VP8X", func(_ *testing.T, w, h int) []byte {
			header := imageHeader("VP8X", w, h)
			const unknownSize = (1 << 20) + 1 // Odd length also exercises RIFF padding.
			body := make([]byte, len(header)+8+unknownSize+1)
			copy(body, header[:12])
			copy(body[12:], "JUNK")
			binary.LittleEndian.PutUint32(body[16:], unknownSize)
			copy(body[20+unknownSize+1:], header[12:])
			binary.LittleEndian.PutUint32(body[4:], uint32(len(body)-8))
			return body
		}, 16000},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			for _, tc := range []struct {
				name      string
				w, h      int
				maxPixels int64
				blocked   bool
			}{
				{"within cap", 2, 3, 6, false},
				{"over cap", fixture.largeDim, fixture.largeDim, 6, true},
			} {
				t.Run(tc.name, func(t *testing.T) {
					body := fixture.body(t, tc.w, tc.h)
					w, h, err := imageDimensions(body, imageFormat(body))
					if err != nil || w != int64(tc.w) || h != int64(tc.h) {
						t.Errorf("late dimensions: got %dx%d err=%v, want %dx%d", w, h, err, tc.w, tc.h)
					}
					calls := 0
					inner := imageTestConverter(func(_ context.Context, got []byte, filename, contentType string) (string, error) {
						calls++
						if !bytes.Equal(got, body) || filename != "original" || contentType != "application/octet-stream" {
							t.Error("changed image arguments")
						}
						return "converted", nil
					})
					md, err := ImagePolicy(inner, true, tc.maxPixels).Convert(context.Background(), body, "original", "application/octet-stream")
					if tc.blocked {
						if err == nil || !strings.Contains(err.Error(), "GKG_CONVERTER_MAX_IMAGE_MP") || calls != 0 || md != "" {
							t.Fatalf("want pixel-cap refusal before inner: calls=%d markdown=%q err=%v", calls, md, err)
						}
					} else if err != nil || calls != 1 || md != "converted" {
						t.Fatalf("valid late header must reach inner: calls=%d markdown=%q err=%v", calls, md, err)
					}
				})
			}
		})
	}
}

func tiffWithLateIFD(format string, w, h int) []byte {
	header := imageHeader(format, w, h)
	// Typical TIFF writers put the first directory after the image strips.
	const offset = 8 + (2 << 20)
	body := make([]byte, offset+len(header)-8)
	copy(body, header[:8])
	copy(body[offset:], header[8:])
	var order binary.ByteOrder = binary.LittleEndian
	if format == "TIFF MM" {
		order = binary.BigEndian
	}
	order.PutUint32(body[4:], offset)
	return body
}

func TestImagePolicyMalformedHeaders(t *testing.T) {
	cases := map[string][]byte{
		"PNG missing IHDR": []byte("\x89PNG\r\n\x1a\n"),
		"JPEG truncated":   []byte{255, 216, 255}, "GIF truncated": []byte("GIF89a"),
		"BMP truncated": []byte("BM"), "WEBP truncated": []byte("RIFF\x00\x00\x00\x00WEBP"),
		"TIFF truncated": []byte("II\x2a\x00"),
	}
	bad := imageHeader("TIFF II", 2, 3)
	binary.LittleEndian.PutUint32(bad[4:], 0xfffffff0)
	cases["TIFF bad offset"] = bad
	bad = imageHeader("TIFF MM", 2, 3)
	binary.BigEndian.PutUint16(bad[8:], 65535)
	cases["TIFF huge entry count"] = bad
	bad = imageHeader("TIFF II", 2, 3)
	binary.LittleEndian.PutUint32(bad[14:], 0xffffffff)
	cases["TIFF huge value count"] = bad
	for _, format := range []string{"VP8", "VP8L", "VP8X"} {
		bad = imageHeader(format, 2, 3)
		binary.LittleEndian.PutUint32(bad[16:], 0xffffffff)
		cases[format+" oversized chunk"] = bad
		bad = imageHeader(format, 2, 3)
		cases[format+" short chunk"] = bad[:len(bad)-2]
	}
	bad = imageHeader("BMP", 2, 3)
	binary.LittleEndian.PutUint32(bad[14:], 0xffffffff)
	cases["BMP huge DIB"] = bad
	cases["PNG missing pixels"] = imageHeader("PNG", 2, 3)
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			inner := imageTestConverter(func(context.Context, []byte, string, string) (string, error) {
				t.Fatal("corrupt image reached inner")
				return "", nil
			})
			_, err := ImagePolicy(inner, true, 12_500_000).Convert(context.Background(), body, "broken", "application/octet-stream")
			if err == nil || !strings.Contains(err.Error(), "unsupported or corrupt image") {
				t.Fatalf("want corrupt-image error, got %v", err)
			}
		})
	}
	// Walk each truncated header to catch unchecked offsets and slicing.
	for _, format := range imageFormats {
		if format == "PNG" {
			continue
		}
		b := imageHeader(format, 2, 3)
		for n := 0; n < len(b); n++ {
			t.Run(fmt.Sprintf("prefix/%s/%d", format, n), func(t *testing.T) {
				inner := imageTestConverter(func(context.Context, []byte, string, string) (string, error) { return "", nil })
				_, _ = ImagePolicy(inner, true, 12_500_000).Convert(context.Background(), b[:n], "broken", "image/unknown")
			})
		}
	}
}

func TestImagePolicyContentTypes(t *testing.T) {
	for _, tc := range []struct {
		name     string
		body     []byte
		ct, want string
	}{
		{"opaque type", encodedImage(t, "PNG", false), "application/octet-stream", ""},
		{"uppercase parameters", encodedImage(t, "PNG", false), " Image/PNG; charset=binary ", ""},
		{"BOM PNG", append([]byte{239, 187, 191}, encodedImage(t, "PNG", false)...), "", ""},
		{"PDF magic wins", []byte("%PDF-1.7"), "image/png", ""},
		{"DOCX magic wins", []byte("PK\x03\x04"), "image/png", ""},
		{"SVG binary", []byte("<svg/>"), " Image/SVG+XML; charset=utf-8 ", "unsupported or corrupt image"},
		{"unknown image", []byte{0, 1, 2}, "image/png", "unsupported or corrupt image"},
		{"HEIC", []byte("\x00\x00\x00\x18ftypheic"), "application/octet-stream", "HEIC/AVIF images are not supported by the converter"},
		{"AVIF", []byte("\x00\x00\x00\x18ftypavif"), "image/avif", "HEIC/AVIF images are not supported by the converter"},
		{"isom video", []byte("\x00\x00\x00\x18ftypisom"), "image/png", ""},
		{"mp41 video", []byte("\x00\x00\x00\x18ftypmp41"), "application/octet-stream", ""},
		{"qt video", []byte("\x00\x00\x00\x18ftypqt  "), "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			inner := imageTestConverter(func(_ context.Context, body []byte, filename, ct string) (string, error) {
				calls++
				if !bytes.Equal(body, tc.body) || filename != "original" || ct != tc.ct {
					t.Error("changed passthrough arguments")
				}
				return "converted", nil
			})
			_, err := ImagePolicy(inner, true, 12_500_000).Convert(context.Background(), tc.body, "original", tc.ct)
			if tc.want == "" {
				if err != nil || calls != 1 {
					t.Fatalf("want pass: calls=%d err=%v", calls, err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.want) || calls != 0 {
				t.Fatalf("want %q before inner: calls=%d err=%v", tc.want, calls, err)
			}
			if (tc.name == "HEIC" || tc.name == "AVIF") && err != nil && !strings.Contains(err.Error(), "export as PNG or JPEG") {
				t.Error("missing export guidance")
			}
		})
	}
}

func TestImagePolicyTransparentPNG(t *testing.T) {
	body := encodedImage(t, "PNG", true)
	calls := 0
	inner := imageTestConverter(func(_ context.Context, got []byte, filename, ct string) (string, error) {
		calls++
		if filename != "diagram.png" || ct != "Image/PNG; custom=1" {
			t.Error("flatten changed filename or content type")
		}
		img, err := png.Decode(bytes.NewReader(got))
		if err != nil {
			t.Fatal(err)
		}
		for y := 0; y < 3; y++ {
			for x := 0; x < 2; x++ {
				_, _, _, a := img.At(x, y).RGBA()
				if a != 65535 {
					t.Error("nonopaque flattened pixel")
				}
			}
		}
		r, g, b, _ := img.At(0, 0).RGBA()
		if r != 65535 || g != 65535 || b != 65535 {
			t.Error("transparent pixel must become white")
		}
		r, g, b, _ = img.At(1, 0).RGBA()
		if r < 32000 || r > 33000 || r != g || r != b {
			t.Error("partial alpha must composite over white")
		}
		return "converted", nil
	})
	if _, err := ImagePolicy(inner, true, 6).Convert(context.Background(), body, "diagram.png", "Image/PNG; custom=1"); err != nil || calls != 1 {
		t.Fatalf("flatten failed: %v calls=%d", err, calls)
	}
}

func TestImagePolicyFlattenCancellation(t *testing.T) {
	imageFlattenSlots <- struct{}{}
	defer func() { <-imageFlattenSlots }()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	inner := imageTestConverter(func(context.Context, []byte, string, string) (string, error) {
		t.Error("canceled image reached inner")
		return "", nil
	})
	_, err := ImagePolicy(inner, true, 6).Convert(ctx, encodedImage(t, "PNG", true), "diagram.png", "image/png")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("waiting for shared flatten slot must respect context: %v", err)
	}
}

func TestImagePolicyTIFFDuplicateDimensions(t *testing.T) {
	// Conflicting tags must not let a later small width hide a large width
	// that a different TIFF decoder could select.
	body := make([]byte, 50)
	copy(body, imageHeader("TIFF II", 16000, 3)[:34])
	binary.LittleEndian.PutUint16(body[8:10], 3)
	copy(body[34:46], body[10:22])
	binary.LittleEndian.PutUint32(body[42:46], 2)
	inner := imageTestConverter(func(context.Context, []byte, string, string) (string, error) {
		t.Error("ambiguous TIFF reached inner")
		return "", nil
	})
	_, err := ImagePolicy(inner, true, 6).Convert(context.Background(), body, "ambiguous.tiff", "image/tiff")
	if err == nil || !strings.Contains(err.Error(), "unsupported or corrupt image") {
		t.Fatalf("want corrupt-image error for duplicate dimensions, got %v", err)
	}
}
