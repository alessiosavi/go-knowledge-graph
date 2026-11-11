package convert

import (
	"context"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/alessiosavi/go-knowledge-graph/internal/config"
)

func doclingConfig(url string) config.Config {
	cfg := config.Config{}
	cfg.Converter.Mode = "docling"
	cfg.Converter.URL = url
	cfg.Converter.TimeoutS = 300
	cfg.Converter.MaxResponseMB = 32
	return cfg
}

func TestNewReturnsNilInterfaceWhenOff(t *testing.T) {
	if c := New(config.Config{}); c != nil {
		t.Errorf("New(zero config) = %#v, want a nil interface", c)
	}
	cfg := config.Config{}
	cfg.Converter.Mode = "off"
	if c := New(cfg); c != nil {
		t.Errorf("New(off) = %#v, want a nil interface", c)
	}
	if c := New(doclingConfig("http://docling.example:5001")); c == nil {
		t.Error("New(docling) = nil, want a converter")
	}
}

func TestDoclingConvertShape(t *testing.T) {
	var gotFilename, gotContentType, gotToFormats, gotKey, gotOCR, gotExport string
	var gotBody []byte
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if r.URL.Path != "/v1/convert/file" {
			t.Errorf("path = %q, want /v1/convert/file", r.URL.Path)
		}
		gotKey = r.Header.Get("X-Api-Key")
		mt, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || mt != "multipart/form-data" {
			t.Errorf("content-type = %q, want multipart/form-data", r.Header.Get("Content-Type"))
		}
		mr := multipart.NewReader(r.Body, params["boundary"])
		for {
			part, err := mr.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Errorf("next part: %v", err)
				break
			}
			data, _ := io.ReadAll(part)
			switch part.FormName() {
			case "files":
				gotFilename = part.FileName()
				gotContentType = part.Header.Get("Content-Type")
				gotBody = data
			case "to_formats":
				gotToFormats = string(data)
			case "ocr_preset":
				gotOCR = string(data)
			case "ocr_engine":
				t.Error("deprecated ocr_engine form field was sent")
			case "image_export_mode":
				gotExport = string(data)
			}
		}
		fmt.Fprint(w, `{"document":{"md_content":"# Converted\n\nbody"},"status":"success"}`)
	}))
	defer ts.Close()

	// A trailing slash must not produce a double-slash endpoint path.
	cfg := doclingConfig(ts.URL + "/")
	cfg.Converter.APIKey = "dk"
	c := New(cfg)
	md, err := c.Convert(context.Background(), []byte("%PDF-1.4 body"), "report.pdf", "application/pdf")
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	if md != "# Converted\n\nbody" {
		t.Errorf("markdown = %q, want the converted body", md)
	}
	if gotFilename != "report.pdf" || gotContentType != "application/pdf" {
		t.Errorf("part = %q (%s), want report.pdf (application/pdf)", gotFilename, gotContentType)
	}
	if gotToFormats != "md" {
		t.Errorf("to_formats = %q, want md", gotToFormats)
	}
	if gotOCR != "tesseract" || gotExport != "placeholder" {
		t.Errorf("ocr_preset=%q image_export_mode=%q, want tesseract and placeholder", gotOCR, gotExport)
	}
	if gotKey != "dk" {
		t.Errorf("X-Api-Key = %q, want the configured key", gotKey)
	}
	if string(gotBody) != "%PDF-1.4 body" {
		t.Errorf("part body = %q, want the raw document bytes", gotBody)
	}
}

func TestDoclingAuthOptional(t *testing.T) {
	var gotKey string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("X-Api-Key")
		_, _ = io.Copy(io.Discard, r.Body)
		fmt.Fprint(w, `{"document":{"md_content":"ok"},"status":"success"}`)
	}))
	defer ts.Close()

	c := New(doclingConfig(ts.URL)) // no key configured
	if _, err := c.Convert(context.Background(), []byte("%PDF-1.4"), "a.pdf", "application/pdf"); err != nil {
		t.Fatalf("Convert: %v", err)
	}
	if gotKey != "" {
		t.Errorf("X-Api-Key = %q, want it omitted when unconfigured", gotKey)
	}
}

func TestDoclingFailureStatus(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"document":{},"status":"failure","errors":["unsupported format: 7z"]}`)
	}))
	defer ts.Close()
	c := New(doclingConfig(ts.URL))
	_, err := c.Convert(context.Background(), []byte("PK\x03\x04x"), "a.7z", "application/x-7z-compressed")
	if err == nil || !strings.Contains(err.Error(), "unsupported format: 7z") {
		t.Fatalf("err = %v, want it to quote the server's errors[0]", err)
	}
}

// TestDoclingFailureStatusWithMarkdown pins that a failure status is a
// failure even when the response still carries content: a partially
// converted document must never be ingested as if it succeeded.
func TestDoclingFailureStatusWithMarkdown(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"document":{"md_content":"# partial content"},"status":"failure","errors":["conversion failed at page 3"]}`)
	}))
	defer ts.Close()
	c := New(doclingConfig(ts.URL))
	_, err := c.Convert(context.Background(), []byte("%PDF-1.4"), "a.pdf", "application/pdf")
	if err == nil {
		t.Fatal("failure status with content: want error")
	}
	if !strings.Contains(err.Error(), "conversion failed at page 3") {
		t.Errorf("err = %v, want it to quote the server's errors[0]", err)
	}
}

func TestDoclingPartialSuccessAccepted(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"document":{"md_content":"partial md"},"status":"partial_success","errors":["page 3 skipped"]}`)
	}))
	defer ts.Close()
	c := New(doclingConfig(ts.URL))
	md, err := c.Convert(context.Background(), []byte("%PDF-1.4"), "a.pdf", "application/pdf")
	if err != nil {
		t.Fatalf("partial_success with markdown = %v, want accepted", err)
	}
	if md != "partial md" {
		t.Errorf("markdown = %q, want the partial content", md)
	}
}

func TestDoclingResponseErrors(t *testing.T) {
	for _, tc := range []struct {
		name, response, wantMD, wantError string
	}{
		{
			name:     "success",
			response: `{"status":"success","document":{"md_content":"complete md"},"errors":[]}`,
			wantMD:   "complete md",
		},
		{
			name:     "partial_success_object_errors",
			response: `{"status":"partial_success","document":{"md_content":"partial md"},"errors":[{"component_type":"model","module_name":"ocr","message":"page 3 skipped"}]}`,
			wantMD:   "partial md",
		},
		{
			name:      "failure_object_errors",
			response:  `{"status":"failure","document":{"md_content":"incomplete md"},"errors":[{"component_type":"model","module_name":"ocr","message":"conversion failed at page 3"}]}`,
			wantError: "conversion failed: conversion failed at page 3",
		},
		{
			name:      "string_errors",
			response:  `{"status":"failure","document":{},"errors":["unsupported format: 7z"]}`,
			wantError: "conversion failed: unsupported format: 7z",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				fmt.Fprint(w, tc.response)
			}))
			defer ts.Close()
			md, err := New(doclingConfig(ts.URL)).Convert(context.Background(), []byte("%PDF-1.4"), "a.pdf", "application/pdf")
			if tc.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantError) {
					t.Fatalf("err = %v, want %q", err, tc.wantError)
				}
				if md != "" {
					t.Fatalf("failed conversion returned markdown %q", md)
				}
				return
			}
			if err != nil || md != tc.wantMD {
				t.Fatalf("Convert = (%q, %v), want (%q, nil)", md, err, tc.wantMD)
			}
		})
	}
}

func TestDoclingEmptyMarkdownFails(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"document":{"md_content":"   "},"status":"success"}`)
	}))
	defer ts.Close()
	c := New(doclingConfig(ts.URL))
	if _, err := c.Convert(context.Background(), []byte("%PDF-1.4"), "a.pdf", "application/pdf"); err == nil {
		t.Fatal("empty markdown: want error")
	}
}

func TestDoclingNon2xxCappedBody(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusGatewayTimeout)
		fmt.Fprint(w, strings.Repeat("x", 6000)+"TAIL")
	}))
	defer ts.Close()
	c := New(doclingConfig(ts.URL))
	_, err := c.Convert(context.Background(), []byte("%PDF-1.4"), "a.pdf", "application/pdf")
	if err == nil || !strings.Contains(err.Error(), "504") {
		t.Fatalf("err = %v, want the HTTP status surfaced", err)
	}
	if strings.Contains(err.Error(), "TAIL") {
		t.Errorf("err = %v, want the body capped at 4KB", err)
	}
}

func TestDoclingResponseCapExceeded(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"document":{"md_content":"`)
		fmt.Fprint(w, strings.Repeat("A", (1<<20)+64))
		fmt.Fprint(w, `"},"status":"success"}`)
	}))
	defer ts.Close()
	cfg := doclingConfig(ts.URL)
	cfg.Converter.MaxResponseMB = 1
	c := New(cfg)
	if _, err := c.Convert(context.Background(), []byte("%PDF-1.4"), "a.pdf", "application/pdf"); err == nil {
		t.Fatal("response beyond the cap: want a decode error")
	}
}

func TestDoclingContextCancel(t *testing.T) {
	// Consume the request body first: Go's server only starts its
	// disconnect-watching background read once a body has been consumed;
	// the release channel is the belt-and-braces unblock for cleanup.
	release := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	t.Cleanup(func() {
		close(release)
		ts.Close()
	})
	c := New(doclingConfig(ts.URL))
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_, err := c.Convert(ctx, []byte("%PDF-1.4"), "a.pdf", "application/pdf")
	if err == nil {
		t.Fatal("want a context error")
	}
	if strings.Contains(err.Error(), "127.0.0.1") {
		t.Errorf("err = %v, want no endpoint value echoed", err)
	}
}

func TestDoclingConfiguredOCR(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Error(err)
			return
		}
		defer r.MultipartForm.RemoveAll()
		if r.FormValue("ocr_preset") != "easyocr" || r.FormValue("ocr_engine") != "" || r.FormValue("image_export_mode") != "placeholder" || r.FormValue("to_formats") != "md" {
			t.Error("configured OCR and safe export fields missing")
		}
		fmt.Fprint(w, `{"status":"success","document":{"md_content":"converted"}}`)
	}))
	defer ts.Close()
	cfg := doclingConfig(ts.URL)
	cfg.Converter.OCREngine = "easyocr"
	if _, err := New(cfg).Convert(context.Background(), []byte("%PDF-1.7"), "report.pdf", "application/pdf"); err != nil {
		t.Fatal(err)
	}
}
