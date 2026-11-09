package ingest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"strings"
	"unicode/utf8"

	"github.com/alessiosavi/go-knowledge-graph/internal/convert"
)

// zipMagic is the local-file magic shared by docx/xlsx/pptx/odt
// containers (the subtype is not recoverable from the magic alone).
var zipMagic = []byte("PK\x03\x04")

// Decode turns a raw body into cleaned text. Dispatch order:
//
//  1. the %PDF magic always wins (a PDF served as application/octet-stream
//     over URL must still decode as PDF);
//  2. an HTML content type or an `<html`/doctype sniff goes through
//     readability;
//  3. text/*, markdown and unknown-but-UTF-8 bodies pass through unchanged
//     (markdown headings stay for the chunker);
//  4. anything else is an undecodable binary.
func Decode(body []byte, contentType string, src *url.URL) (Loaded, error) {
	// A UTF-8 BOM must not defeat the magic and sniff checks (the upload
	// path has no content type to lean on).
	body = bytes.TrimPrefix(body, []byte{0xEF, 0xBB, 0xBF})
	if len(body) == 0 {
		return Loaded{}, errors.New("ingest: decode: empty body")
	}
	if bytes.HasPrefix(body, []byte("%PDF")) {
		text, title, err := PDFToText(body)
		if err != nil {
			return Loaded{}, err
		}
		return Loaded{Text: text, Title: title, Hash: sha256Hex(text)}, nil
	}

	ct := strings.ToLower(strings.TrimSpace(strings.Split(contentType, ";")[0]))
	if ct == "text/html" || ct == "application/xhtml+xml" || looksLikeHTML(body) {
		text, err := HTMLToText(body, src)
		if err != nil {
			return Loaded{}, fmt.Errorf("ingest: decode html: %w", err)
		}
		text = strings.TrimSpace(text)
		if text == "" {
			return Loaded{}, errors.New("ingest: decode html: no text content")
		}
		return Loaded{Text: text, Hash: sha256Hex(text)}, nil
	}

	if strings.HasPrefix(ct, "text/") || ct == "application/markdown" || utf8.Valid(body) {
		text := string(body)
		return Loaded{Text: text, Hash: sha256Hex(text)}, nil
	}
	return Loaded{}, fmt.Errorf("ingest: decode: undecodable binary content (content-type %q)", contentType)
}

// looksLikeHTML sniffs a leading doctype or <html tag (with a tag
// boundary, so plain text like "<htmlfoo" passes through) on the trimmed
// body.
func looksLikeHTML(body []byte) bool {
	head := body
	if len(head) > 512 {
		head = head[:512]
	}
	trimmed := strings.ToLower(strings.TrimSpace(string(head)))
	if strings.HasPrefix(trimmed, "<!doctype") {
		return true
	}
	rest, ok := strings.CutPrefix(trimmed, "<html")
	if !ok {
		return false
	}
	return rest == "" || strings.HasPrefix(rest, ">") || strings.HasPrefix(rest, "/") ||
		strings.HasPrefix(rest, " ") || strings.HasPrefix(rest, "\t") || strings.HasPrefix(rest, "\n")
}

// DefaultLoader returns the production TextLoader. Dispatch honors an
// explicit document kind: kind=file/text inputs are never fetched (a
// URL-shaped source from a file submission must not become a network
// request), and reloads of file/text documents fail with a resubmit hint.
// URL inputs fetch through the SSRF-guarded fetcher; field precedence
// mirrors resolveIdentity (URL first, then bytes, then inline text).
//
// A nil conv keeps today's behavior byte for byte. With a converter
// attached, rich binary formats (PDF, zip-based Office documents, images,
// and any binary Decode would reject) convert to markdown first and the
// markdown enters the pipeline unchanged — no silent fallback on failure.
func DefaultLoader(f *Fetcher, conv convert.Converter) TextLoader {
	return func(ctx context.Context, in Input) (Loaded, error) {
		kind := in.Kind
		if kind == "" {
			switch {
			case in.URL != "":
				kind = "url"
			case len(in.Bytes) > 0:
				kind = "file"
			default:
				kind = "text"
			}
		}
		switch kind {
		case "url":
			if in.URL == "" {
				return Loaded{}, fmt.Errorf("ingest: url input without a URL")
			}
			u, uerr := url.Parse(in.URL)
			if uerr != nil || (u.Scheme != "http" && u.Scheme != "https") {
				return Loaded{}, fmt.Errorf("ingest: cannot reload url input %q after restart; resubmit the content", in.Source)
			}
			if f == nil {
				return Loaded{}, errors.New("ingest: url input without a fetcher")
			}
			res, err := f.Fetch(ctx, in.URL)
			if err != nil {
				return Loaded{}, err
			}
			src, err := url.Parse(res.FinalURL)
			if err != nil {
				src = nil
			}
			l, err := decodeBody(ctx, conv, res.Body, res.ContentType, src, "url", res.FinalURL)
			if err != nil {
				return Loaded{}, err
			}
			if l.Title == "" {
				l.Title = in.Title
			}
			return l, nil
		case "file":
			if len(in.Bytes) == 0 {
				return Loaded{}, fmt.Errorf("ingest: cannot reload file input %q after restart; resubmit the content", in.Source)
			}
			l, err := decodeBody(ctx, conv, in.Bytes, "", nil, "file", in.Source)
			if err != nil {
				return Loaded{}, err
			}
			if l.Title == "" {
				l.Title = in.Title
			}
			return l, nil
		case "text":
			if in.Text == "" {
				return Loaded{}, fmt.Errorf("ingest: cannot reload text input %q after restart; resubmit the content", in.Source)
			}
			return Loaded{Text: in.Text, Title: in.Title, Hash: sha256Hex(in.Text)}, nil
		default:
			return Loaded{}, fmt.Errorf("ingest: unknown input kind %q", kind)
		}
	}
}

// decodeBody decodes a fetched or submitted body: with a converter
// attached, rich binary formats convert to markdown first (the markdown's
// sha256 is the content hash); everything else keeps the existing Decode
// dispatch byte for byte.
func decodeBody(ctx context.Context, conv convert.Converter, body []byte, contentType string, src *url.URL, kind, source string) (Loaded, error) {
	if conv != nil && converterRoute(body, contentType) {
		md, err := conv.Convert(ctx, body, converterFilename(kind, source, contentType, body), converterContentType(kind, contentType, body))
		if err != nil {
			return Loaded{}, fmt.Errorf("ingest: convert: %w", err)
		}
		return Loaded{Text: md, Hash: sha256Hex(md)}, nil
	}
	return Decode(body, contentType, src)
}

// converterRoute reports whether a body goes through the converter when one
// is configured. It mirrors Decode's dispatch: PDF, zip-based and image
// magic, plus any binary Decode would reject; HTML and UTF-8 text keep
// their existing paths.
func converterRoute(body []byte, contentType string) bool {
	b := bytes.TrimPrefix(body, []byte{0xEF, 0xBB, 0xBF})
	if len(b) == 0 {
		return false
	}
	if bytes.HasPrefix(b, []byte("%PDF")) || bytes.HasPrefix(b, zipMagic) || imageExtension(b) != "" {
		return true
	}
	ct := strings.ToLower(strings.TrimSpace(strings.Split(contentType, ";")[0]))
	if ct == "text/html" || ct == "application/xhtml+xml" || looksLikeHTML(b) {
		return false
	}
	if strings.HasPrefix(ct, "text/") || ct == "application/markdown" || utf8.Valid(b) {
		return false
	}
	return true
}

// converterFilename derives the multipart filename so docling picks the
// right parser: file inputs keep their submitted name; URL inputs use the
// path basename when it carries an extension, else the basename with the
// sniffed canonical extension ("document.<ext>" for opaque paths).
func converterFilename(kind, source, contentType string, body []byte) string {
	switch kind {
	case "file":
		if source != "" {
			return path.Base(source)
		}
	case "url":
		base := ""
		if u, err := url.Parse(source); err == nil {
			base = path.Base(u.Path)
		}
		if base == "." || base == "/" {
			base = ""
		}
		if base != "" && path.Ext(base) != "" {
			return base
		}
		if base == "" {
			base = "document"
		}
		if ext := converterExtension(contentType, body); ext != "" {
			return base + ext
		}
		return base
	}
	if ext := converterExtension(contentType, body); ext != "" {
		return "document" + ext
	}
	return "document.bin"
}

// converterContentType returns the multipart part's content type: URL
// fetches carry the served type; file inputs (and empty fetches) detect
// from the bytes, so the part type is never empty.
func converterContentType(kind, fetched string, body []byte) string {
	if kind == "url" && strings.TrimSpace(fetched) != "" {
		return fetched
	}
	return http.DetectContentType(body)
}

// converterExtension maps a trusted content type (or the body magic) to a
// canonical extension.
func converterExtension(contentType string, body []byte) string {
	ct := strings.ToLower(strings.TrimSpace(strings.Split(contentType, ";")[0]))
	switch ct {
	case "application/pdf":
		return ".pdf"
	case "application/vnd.openxmlformats-officedocument.wordprocessingml.document":
		return ".docx"
	case "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet":
		return ".xlsx"
	case "application/vnd.openxmlformats-officedocument.presentationml.presentation":
		return ".pptx"
	case "application/vnd.oasis.opendocument.text":
		return ".odt"
	case "image/png":
		return ".png"
	case "image/jpeg":
		return ".jpg"
	case "image/tiff":
		return ".tiff"
	case "image/webp":
		return ".webp"
	case "image/bmp":
		return ".bmp"
	case "image/gif":
		return ".gif"
	}
	return imageExtension(body)
}

// imageExtension sniffs the image magic; zip bodies map to the canonical
// OOXML extension (.docx — the container subtype is not recoverable from
// the magic alone, and a served content type wins upstream).
func imageExtension(body []byte) string {
	switch {
	case bytes.HasPrefix(body, []byte{0x89, 'P', 'N', 'G'}):
		return ".png"
	case bytes.HasPrefix(body, []byte{0xFF, 0xD8, 0xFF}):
		return ".jpg"
	case bytes.HasPrefix(body, []byte{'I', 'I', 0x2A, 0x00}) || bytes.HasPrefix(body, []byte{'M', 'M', 0x00, 0x2A}):
		return ".tiff"
	case len(body) >= 12 && bytes.HasPrefix(body, []byte("RIFF")) && bytes.Equal(body[8:12], []byte("WEBP")):
		return ".webp"
	case bytes.HasPrefix(body, []byte{'B', 'M'}):
		return ".bmp"
	case bytes.HasPrefix(body, []byte("GIF87a")) || bytes.HasPrefix(body, []byte("GIF89a")):
		return ".gif"
	case bytes.HasPrefix(body, zipMagic):
		return ".docx"
	}
	return ""
}
