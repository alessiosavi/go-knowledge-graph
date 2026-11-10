package convert

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	_ "image/gif"
	_ "image/jpeg"
	"image/png"
	"strings"
)

// ImagePolicy refuses disabled, unsupported or oversized images before the
// wrapped converter runs. PNG transparency is composited onto white after
// checking dimensions. Non-images retain their original converter arguments.
func ImagePolicy(c Converter, enabled bool, maxPixels int64) Converter {
	return imagePolicy{inner: c, enabled: enabled, maxPixels: maxPixels}
}

type imagePolicy struct {
	inner     Converter
	enabled   bool
	maxPixels int64
}

var errImageHeader = errors.New("unsupported or corrupt image")

// A single shared slot bounds PNG decoding/compositing memory across all
// converters and ingest workers. It is released before contacting docling.
var imageFlattenSlots = make(chan struct{}, 1)

func (c imagePolicy) Convert(ctx context.Context, body []byte, filename, contentType string) (string, error) {
	b := bytes.TrimPrefix(body, []byte{0xEF, 0xBB, 0xBF})
	ct := strings.ToLower(strings.TrimSpace(strings.SplitN(contentType, ";", 2)[0]))
	format := imageFormat(b)
	// A recognized non-image signature wins over a misleading content type.
	if format == "document" || (format == "" && !strings.HasPrefix(ct, "image/")) {
		return c.inner.Convert(ctx, body, filename, contentType)
	}
	if !c.enabled {
		return "", errors.New("image conversion is disabled (set GKG_CONVERTER_IMAGES=true to send images to docling)")
	}
	if format == "heif" {
		return "", errors.New("HEIC/AVIF images are not supported by the converter — export as PNG or JPEG")
	}
	w, h, err := imageDimensions(b, format)
	if err != nil || w <= 0 || h <= 0 {
		return "", errImageHeader
	}
	// Division avoids overflowing even for TIFF's uint32 dimensions.
	if c.maxPixels <= 0 || w > c.maxPixels/h {
		return "", fmt.Errorf("image is %d×%d (%g MP); the limit is %g MP (GKG_CONVERTER_MAX_IMAGE_MP) — downscale it", w, h, float64(w)*float64(h)/1e6, float64(c.maxPixels)/1e6)
	}
	if format == "png" {
		flattened, err := flattenPNG(ctx, b)
		if err != nil {
			return "", err
		}
		if flattened != nil {
			body = flattened
		}
	}
	return c.inner.Convert(ctx, body, filename, contentType)
}

func imageFormat(b []byte) string {
	switch {
	case bytes.HasPrefix(b, []byte("%PDF")), bytes.HasPrefix(b, []byte("PK\x03\x04")):
		return "document"
	case bytes.HasPrefix(b, []byte{0x89, 'P', 'N', 'G'}):
		return "png"
	case bytes.HasPrefix(b, []byte{0xFF, 0xD8, 0xFF}):
		return "jpeg"
	case bytes.HasPrefix(b, []byte("GIF87a")), bytes.HasPrefix(b, []byte("GIF89a")):
		return "gif"
	case bytes.HasPrefix(b, []byte("BM")):
		return "bmp"
	case bytes.HasPrefix(b, []byte{'I', 'I', 42, 0}), bytes.HasPrefix(b, []byte{'M', 'M', 0, 42}):
		return "tiff"
	case len(b) >= 12 && bytes.Equal(b[:4], []byte("RIFF")) && bytes.Equal(b[8:12], []byte("WEBP")):
		return "webp"
	case len(b) >= 12 && bytes.Equal(b[4:8], []byte("ftyp")):
		switch string(b[8:12]) {
		case "avif", "avis", "heic", "heix", "heim", "heis", "hevc", "hevx", "hevm", "hevs", "mif1", "msf1":
			return "heif"
		case "isom", "mp41", "mp42", "qt  ":
			return "document"
		}
	}
	return ""
}

// Header parsing is bounded by the supplied body. Readers never allocate
// storage using untrusted chunk lengths, dimensions or directory counts.
func imageDimensions(b []byte, format string) (int64, int64, error) {
	switch format {
	case "png", "jpeg", "gif":
		cfg, _, err := image.DecodeConfig(bytes.NewReader(b))
		return int64(cfg.Width), int64(cfg.Height), err
	case "bmp":
		return bmpDimensions(b)
	case "webp":
		return webpDimensions(b)
	case "tiff":
		return tiffDimensions(b)
	default:
		return 0, 0, errImageHeader
	}
}

func bmpDimensions(b []byte) (int64, int64, error) {
	if len(b) < 26 {
		return 0, 0, errImageHeader
	}
	size := uint64(binary.LittleEndian.Uint32(b[14:18]))
	if size > uint64(len(b)-14) {
		return 0, 0, errImageHeader
	}
	if size == 12 {
		return int64(binary.LittleEndian.Uint16(b[18:20])), int64(binary.LittleEndian.Uint16(b[20:22])), nil
	}
	if size < 40 {
		return 0, 0, errImageHeader
	}
	w := int64(int32(binary.LittleEndian.Uint32(b[18:22])))
	h := int64(int32(binary.LittleEndian.Uint32(b[22:26])))
	if h < 0 {
		h = -h
	} // top-down BMP
	return w, h, nil
}

func webpDimensions(b []byte) (int64, int64, error) {
	if len(b) < 20 {
		return 0, 0, errImageHeader
	}
	end := uint64(binary.LittleEndian.Uint32(b[4:8])) + 8
	if end > uint64(len(b)) || end < 20 {
		return 0, 0, errImageHeader
	}
	for pos := uint64(12); pos+8 <= end; {
		size := uint64(binary.LittleEndian.Uint32(b[pos+4 : pos+8]))
		start := pos + 8
		next := start + size + (size & 1)
		if next > end {
			return 0, 0, errImageHeader
		}
		chunk := b[start : start+size]
		switch string(b[pos : pos+4]) {
		case "VP8 ":
			if len(chunk) < 10 || chunk[0]&1 != 0 || !bytes.Equal(chunk[3:6], []byte{0x9d, 1, 0x2a}) {
				return 0, 0, errImageHeader
			}
			return int64(binary.LittleEndian.Uint16(chunk[6:8]) & 0x3fff), int64(binary.LittleEndian.Uint16(chunk[8:10]) & 0x3fff), nil
		case "VP8L":
			if len(chunk) < 5 || chunk[0] != 0x2f {
				return 0, 0, errImageHeader
			}
			bits := binary.LittleEndian.Uint32(chunk[1:5])
			if bits>>29 != 0 {
				return 0, 0, errImageHeader
			}
			return int64(bits&0x3fff) + 1, int64((bits>>14)&0x3fff) + 1, nil
		case "VP8X":
			if len(chunk) != 10 {
				return 0, 0, errImageHeader
			}
			w := uint32(chunk[4]) | uint32(chunk[5])<<8 | uint32(chunk[6])<<16
			h := uint32(chunk[7]) | uint32(chunk[8])<<8 | uint32(chunk[9])<<16
			return int64(w) + 1, int64(h) + 1, nil
		}
		pos = next
	}
	return 0, 0, errImageHeader
}

func tiffDimensions(b []byte) (int64, int64, error) {
	if len(b) < 8 {
		return 0, 0, errImageHeader
	}
	var order binary.ByteOrder = binary.LittleEndian
	if b[0] == 'M' {
		order = binary.BigEndian
	}
	offset := uint64(order.Uint32(b[4:8]))
	end := uint64(len(b))
	if offset < 8 || offset+2 > end {
		return 0, 0, errImageHeader
	}
	count := uint64(order.Uint16(b[offset : offset+2]))
	if offset+2+count*12+4 > end {
		return 0, 0, errImageHeader
	}
	var w, h int64
	for i := uint64(0); i < count; i++ {
		pos := offset + 2 + i*12
		e := b[pos : pos+12]
		tag := order.Uint16(e[:2])
		if tag != 256 && tag != 257 {
			continue
		}
		if order.Uint32(e[4:8]) != 1 {
			return 0, 0, errImageHeader
		}
		var value int64
		switch order.Uint16(e[2:4]) {
		case 3:
			value = int64(order.Uint16(e[8:10]))
		case 4:
			value = int64(order.Uint32(e[8:12]))
		default:
			return 0, 0, errImageHeader
		}
		if value <= 0 || (tag == 256 && w != 0) || (tag == 257 && h != 0) {
			return 0, 0, errImageHeader
		}
		if tag == 256 {
			w = value
		} else {
			h = value
		}
	}
	return w, h, nil
}

func flattenPNG(ctx context.Context, b []byte) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	select {
	case imageFlattenSlots <- struct{}{}:
		defer func() { <-imageFlattenSlots }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	img, err := png.Decode(bytes.NewReader(b))
	if err != nil {
		return nil, errImageHeader
	}
	if opaque, ok := img.(interface{ Opaque() bool }); ok && opaque.Opaque() {
		return nil, nil
	}
	canvas := image.NewRGBA(img.Bounds())
	draw.Draw(canvas, canvas.Bounds(), image.NewUniform(color.White), image.Point{}, draw.Src)
	draw.Draw(canvas, canvas.Bounds(), img, img.Bounds().Min, draw.Over)
	var out bytes.Buffer
	if err := png.Encode(&out, canvas); err != nil {
		return nil, errImageHeader
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}
