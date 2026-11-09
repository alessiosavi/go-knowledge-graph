package ingest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"unicode/utf8"
)

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
func DefaultLoader(f *Fetcher) TextLoader {
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
			l, err := Decode(res.Body, res.ContentType, src)
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
			l, err := Decode(in.Bytes, "", nil)
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
