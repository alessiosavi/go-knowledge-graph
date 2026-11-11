package main

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestStubStreamsSeparatedFrames(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(completions))
	defer srv.Close()
	res, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{"stream":true}`))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	scan := bufio.NewScanner(res.Body)
	var times []time.Time
	var last string
	for scan.Scan() {
		if line := scan.Text(); strings.HasPrefix(line, "data:") {
			times = append(times, time.Now())
			last = line
			if strings.Contains(line, "tool_calls") {
				t.Error("stub must never call tools")
			}
		}
	}
	if scan.Err() != nil || len(times) < 3 || last != "data: [DONE]" {
		t.Fatal("missing completion frames")
	}
	for i := 1; i < len(times); i++ {
		if times[i].Sub(times[i-1]) < 200*time.Millisecond {
			t.Error("frames were buffered or not separated by 200ms")
		}
	}
	res2, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	defer res2.Body.Close()
	data, _ := io.ReadAll(res2.Body)
	var body struct {
		Choices []struct {
			Message struct {
				Content   string
				ToolCalls []any `json:"tool_calls"`
			}
		}
	}
	if json.Unmarshal(data, &body) != nil || len(body.Choices) != 1 || body.Choices[0].Message.Content == "" || len(body.Choices[0].Message.ToolCalls) != 0 {
		t.Fatal("expected one OpenAI completion without tools")
	}
}

func TestStubHoldsRenewalStream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(completions))
	defer srv.Close()
	res, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{"stream":true,"messages":[{"role":"user","content":"edge-check held renewal"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	scan := bufio.NewScanner(res.Body)
	var start time.Time
	frames := 0
	for scan.Scan() {
		if strings.HasPrefix(scan.Text(), "data:") {
			frames++
			if frames == 1 {
				start = time.Now() // measure the hold from the first frame
			}
			if frames == 2 {
				break
			}
		}
	}
	// The stub holds 12 s; allow 1 s of scheduling jitter on loaded runners.
	if frames < 2 || time.Since(start) < 11*time.Second {
		t.Fatal("renewal stream must stay open after the first frame for 12 seconds")
	}
}
