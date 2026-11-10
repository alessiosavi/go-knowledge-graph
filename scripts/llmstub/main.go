// llmstub is an offline OpenAI completion server for the isolated edge check.
package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"
)

func completions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.URL.Path != "/v1/chat/completions" {
		http.NotFound(w, r)
		return
	}
	var request struct {
		Stream bool `json:"stream"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20)).Decode(&request) != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	if !request.Stream {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"edge-check","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"Offline edge check completed."},"finish_reason":"stop"}]}`)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	frames := []string{
		`{"id":"edge-check","choices":[{"index":0,"delta":{"role":"assistant","content":"Offline "}}]}`,
		`{"id":"edge-check","choices":[{"index":0,"delta":{"content":"edge check "}}]}`,
		`{"id":"edge-check","choices":[{"index":0,"delta":{"content":"completed."},"finish_reason":"stop"}]}`,
		`[DONE]`,
	}
	for i, frame := range frames {
		if i > 0 {
			select {
			case <-r.Context().Done():
				return
			case <-time.After(300 * time.Millisecond):
			}
		}
		if _, err := fmt.Fprintf(w, "data: %s\n\n", frame); err != nil {
			return
		}
		if http.NewResponseController(w).Flush() != nil {
			return
		}
	}
}

func main() {
	server := &http.Server{Addr: ":8099", Handler: http.HandlerFunc(completions), ReadHeaderTimeout: 5 * time.Second}
	log.Fatal(server.ListenAndServe())
}
