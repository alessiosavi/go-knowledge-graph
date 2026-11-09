package convert

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"strings"
	"time"

	"github.com/alessiosavi/go-knowledge-graph/internal/config"
)

// doclingErrorBodyCap mirrors the other HTTP clients' hygiene.
const doclingErrorBodyCap = 4 << 10 // 4KB

// Docling is the docling-serve client (stable v1 sync endpoint). Safe for
// concurrent use.
type Docling struct {
	client   *http.Client
	url      string
	apiKey   string
	maxBytes int64
}

// newDocling builds the client from Converter config; config.Load defaults
// TimeoutS (300) and MaxResponseMB (32), and zero values fall back here.
func newDocling(c config.ConverterConfig) *Docling {
	timeoutS := c.TimeoutS
	if timeoutS <= 0 {
		timeoutS = 300
	}
	maxMB := c.MaxResponseMB
	if maxMB <= 0 {
		maxMB = 32
	}
	return &Docling{
		client:   &http.Client{Timeout: time.Duration(timeoutS) * time.Second},
		url:      strings.TrimRight(c.URL, "/") + "/v1/convert/file",
		apiKey:   c.APIKey,
		maxBytes: int64(maxMB) << 20,
	}
}

// Convert posts the document to POST /v1/convert/file as multipart and
// returns the markdown. The filename and content type are always sent:
// docling picks its parser from them. A failure status or an empty result
// is an error quoting the server's reason; partial_success with markdown is
// accepted with a warning. Non-2xx responses surface the capped body (the
// informative sync-timeout 504 included).
func (d *Docling) Convert(ctx context.Context, body []byte, filename, contentType string) (string, error) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	h := make(textproto.MIMEHeader)
	h.Set("Content-Disposition", fmt.Sprintf(`form-data; name="files"; filename=%q`, filename))
	if contentType != "" {
		h.Set("Content-Type", contentType)
	}
	part, err := mw.CreatePart(h)
	if err != nil {
		return "", fmt.Errorf("convert: docling: encode request: %w", err)
	}
	if _, err := part.Write(body); err != nil {
		return "", fmt.Errorf("convert: docling: encode request: %w", err)
	}
	if err := mw.WriteField("to_formats", "md"); err != nil {
		return "", fmt.Errorf("convert: docling: encode request: %w", err)
	}
	if err := mw.Close(); err != nil {
		return "", fmt.Errorf("convert: docling: encode request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.url, &buf)
	if err != nil {
		return "", fmt.Errorf("convert: docling: build request: %w", err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	if d.apiKey != "" {
		req.Header.Set("X-Api-Key", d.apiKey)
	}
	resp, err := d.client.Do(req)
	if err != nil {
		// Never echo the endpoint: classify the transport failure instead.
		switch {
		case errors.Is(err, context.Canceled):
			return "", fmt.Errorf("convert: docling: request canceled: %w", context.Canceled)
		case errors.Is(err, context.DeadlineExceeded):
			return "", fmt.Errorf("convert: docling: request timed out: %w", context.DeadlineExceeded)
		default:
			return "", errors.New("convert: docling: request failed (endpoint unreachable or invalid)")
		}
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, doclingErrorBodyCap))
		return "", fmt.Errorf("convert: docling: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	var parsed struct {
		Document struct {
			MDContent string `json:"md_content"`
		} `json:"document"`
		Status string   `json:"status"`
		Errors []string `json:"errors"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, d.maxBytes)).Decode(&parsed); err != nil {
		return "", fmt.Errorf("convert: docling: decode response: %w", err)
	}
	md := parsed.Document.MDContent
	if strings.TrimSpace(md) == "" {
		reason := parsed.Status
		if len(parsed.Errors) > 0 {
			reason = parsed.Errors[0]
		}
		if reason == "" {
			reason = "empty markdown"
		}
		return "", fmt.Errorf("convert: docling: conversion failed: %s", reason)
	}
	if parsed.Status == "partial_success" {
		slog.Warn("convert: docling returned a partial conversion", "errors", parsed.Errors)
	}
	return md, nil
}
