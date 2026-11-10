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
