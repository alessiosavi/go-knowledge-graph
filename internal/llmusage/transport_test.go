package llmusage

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
)

func testTransport(t *testing.T, l *Ledger, srv *httptest.Server, key CallIDKey) *Transport {
	t.Helper()
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	tr, err := NewTransport(l, u.Host, key)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(tr.CloseIdleConnections)
	return tr
}

func testSend(t *testing.T, tr *Transport, url, id, body string) (*http.Response, error) {
	t.Helper()
	req, err := http.NewRequestWithContext(WithID(t.Context(), id), http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer test-credential")
	return (&http.Client{Transport: tr}).Do(req)
}

func testBody(t *testing.T, resp *http.Response, err error) string {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestTransportCallIDKeyForSmokeFamily(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{}`)
	}))
	defer srv.Close()
	for _, tc := range []struct{ key, otherKey CallIDKey }{
		{ItemIDKey, AskIDKey},
		{AskIDKey, ItemIDKey},
	} {
		t.Run(string(tc.key), func(t *testing.T) {
			run := testRun(t, 2)
			run.Family = "S"
			tr := testTransport(t, openRun(t, run), srv, tc.key)
			resp, err := testSend(t, tr, srv.URL, "smoke", "{}")
			testBody(t, resp, err)
			calls := tr.TakeCalls("smoke")
			if len(calls) != 1 {
				t.Fatalf("calls = %+v", calls)
			}
			raw, err := json.Marshal(calls[0])
			if err != nil {
				t.Fatal(err)
			}
			var fields map[CallIDKey]json.RawMessage
			if err := json.Unmarshal(raw, &fields); err != nil {
				t.Fatal(err)
			}
			if string(fields[tc.key]) != `"smoke"` || fields[tc.otherKey] != nil {
				t.Fatalf("call = %s, want only the %s ID key", raw, tc.key)
			}
		})
	}
}

func TestTransportReservesBeforeSendAndSettlesUsage(t *testing.T) {
	run := testRun(t, 2)
	const request = `{"stream":false,"tools":[{}],"response_format":{"type":"json_object"},"messages":[{"role":"user","content":"private-request"}]}`
	const response = `{"choices":[{"message":{"content":"private-response","tool_calls":[{"extra_content": {"signature":"private-signature"} }]}}],"usage":{"prompt_tokens":10,"completion_tokens":2,"completion_tokens_details":{"reasoning_tokens":3},"total_tokens":14}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rows := ledgerRows(t, run.Path, "reserve")
		want := 1 + float64(len(request))*10/1e6
		if len(rows) != 1 || rows[0]["id"] != "item" || rows[0]["reserved_usd"] != want || rows[0]["settled_usd"] != nil {
			t.Errorf("reserve rows at HTTP arrival = %v, want one at %v", rows, want)
		}
		body, _ := io.ReadAll(r.Body)
		if string(body) != request || r.Header.Get("Authorization") != "Bearer test-credential" {
			t.Error("transport changed HTTP request")
		}
		fmt.Fprint(w, response)
	}))
	defer srv.Close()
	tr := testTransport(t, openRun(t, run), srv, ItemIDKey)
	resp, err := testSend(t, tr, srv.URL, "item", request)
	if got := testBody(t, resp, err); got != response {
		t.Fatalf("transport changed response: %s", got)
	}
	calls := tr.TakeCalls("item")
	if len(calls) != 1 || calls[0].ItemID != "item" || calls[0].Status != 200 || calls[0].Stream ||
		!calls[0].Tools || calls[0].ResponseFormat != "json_object" || calls[0].ToolCalls != 1 ||
		!calls[0].SignatureIn || calls[0].SignatureEchoed != nil || calls[0].Usage == nil ||
		*calls[0].Usage != (Usage{Prompt: 10, Completion: 2, Reasoning: 3, Total: 14}) {
		t.Fatalf("calls = %+v", calls)
	}
	if rows := ledgerRows(t, run.Path, "settle"); len(rows) != 1 || rows[0]["settled_usd"] != 0.4001 {
		t.Fatalf("settle = %v", rows)
	}
	report, err := json.Marshal(calls)
	if err != nil {
		t.Fatal(err)
	}
	both := ledgerText(t, run.Path) + string(report)
	for _, forbidden := range []string{"Authorization", "test-credential", "private-request", "private-response", "private-signature"} {
		if strings.Contains(both, forbidden) {
			t.Errorf("record leaked %s", forbidden)
		}
	}
}

func TestTransportMissingUsageRetainsReserveAndStopsLaterSends(t *testing.T) {
	run := testRun(t, 2)
	run.AllotmentUSD = "1.5"
	var sends atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		sends.Add(1)
		fmt.Fprint(w, `{"choices":[]}`)
	}))
	defer srv.Close()
	tr := testTransport(t, openRun(t, run), srv, ItemIDKey)
	resp, err := testSend(t, tr, srv.URL, "item", "{}")
	testBody(t, resp, err)
	if rows := ledgerRows(t, run.Path, "settle"); len(rows) != 1 || rows[0]["settled_usd"] != nil {
		t.Fatalf("settle = %v", rows)
	}
	for _, id := range []string{"item", "next"} {
		if _, err := testSend(t, tr, srv.URL, id, "{}"); !errors.Is(err, ErrLedgerStop) {
			t.Fatalf("send for %s = %v", id, err)
		}
	}
	if n := sends.Load(); n != 1 {
		t.Fatalf("sent %d requests", n)
	}
	if rows := ledgerRows(t, run.Path, "reserve"); len(rows) != 1 {
		t.Fatalf("reserve rows = %v", rows)
	}
	if calls := tr.TakeCalls("item"); len(calls) != 2 || calls[0].Usage != nil || calls[1].Status != 0 {
		t.Fatalf("calls = %+v", calls)
	}
}

func TestTransportOtherHostPassesThrough(t *testing.T) {
	run := testRun(t, 2)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"access_token":"test-only-token"}`)
	}))
	defer srv.Close()
	tr, err := NewTransport(openRun(t, run), "unused.invalid", ItemIDKey)
	if err != nil {
		t.Fatal(err)
	}
	defer tr.CloseIdleConnections()
	resp, err := testSend(t, tr, srv.URL, "", "{}")
	if got := testBody(t, resp, err); !strings.Contains(got, "test-only-token") {
		t.Fatalf("response = %q", got)
	}
	if len(tr.TakeCalls("")) != 0 || len(ledgerRows(t, run.Path, "reserve")) != 0 {
		t.Fatal("recorded an off-host request")
	}
}

func TestTransportSSESettlesAtEOFAndCountsUniqueToolCalls(t *testing.T) {
	run := testRun(t, 2)
	run.Family = "A"
	const frames = "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"tool\"}]}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"extra_content\":{\"signature\":\"one\"}}]}}]}\n\n" +
		"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":2,\"total_tokens\":14}}\n\n" +
		"data: [DONE]\n\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, frames)
	}))
	defer srv.Close()
	tr := testTransport(t, openRun(t, run), srv, AskIDKey)
	resp, err := testSend(t, tr, srv.URL, "ask", `{"stream":true}`)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if got := ledgerRows(t, run.Path, "settle"); len(got) != 0 {
		t.Fatalf("settled before consuming stream: %v", got)
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil || string(raw) != frames {
		t.Fatalf("stream = %q, %v", raw, err)
	}
	if rows := ledgerRows(t, run.Path, "settle"); len(rows) != 1 || rows[0]["settled_usd"] != 0.4001 {
		t.Fatalf("settle at EOF = %v", rows)
	}
	if calls := tr.TakeCalls("ask"); len(calls) != 1 || calls[0].AskID != "ask" || !calls[0].Stream ||
		!calls[0].SignatureIn || calls[0].ToolCalls != 1 || calls[0].Usage == nil || calls[0].Usage.Total != 14 {
		t.Fatalf("calls = %+v", calls)
	}
}

func TestTransportSignatureEchoExactBytesAndOnlyPreviousCall(t *testing.T) {
	run := testRun(t, 6)
	run.AllotmentUSD = "6"
	var count atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		switch count.Add(1) {
		case 1, 2:
			fmt.Fprint(w, `{"choices":[{"message":{"tool_calls":[{"extra_content":{"sig": "a"}},{"extra_content":{"sig":"b"}}]}}],"usage":{"total_tokens":0}}`)
		default:
			fmt.Fprint(w, `{"choices":[{"message":{"tool_calls":[{"extra_content":{}},{"extra_content":null}]}}],"usage":{"total_tokens":0}}`)
		}
	}))
	defer srv.Close()
	tr := testTransport(t, openRun(t, run), srv, ItemIDKey)
	for _, body := range []string{
		`{}`,
		`{"messages":[{"role":"assistant","tool_calls":[{"extra_content":{"sig": "a"}},{"extra_content":{"sig":"b"}}]}]}`,
		`{"messages":[{"role":"assistant","tool_calls":[{"extra_content":{"sig":"a"}},{"extra_content":{"sig":"b"}}]}]}`,
		`{}`,
	} {
		resp, err := testSend(t, tr, srv.URL, "item", body)
		testBody(t, resp, err)
	}
	c := tr.TakeCalls("item")
	if len(c) != 4 || !c[0].SignatureIn || c[0].SignatureEchoed != nil ||
		!c[1].SignatureIn || c[1].SignatureEchoed == nil || !*c[1].SignatureEchoed ||
		c[2].SignatureIn || c[2].SignatureEchoed == nil || *c[2].SignatureEchoed ||
		c[3].SignatureIn || c[3].SignatureEchoed != nil {
		t.Fatalf("calls = %+v", c)
	}
}

func TestTransportSSECloseWithoutUsageKeepsReservation(t *testing.T) {
	run := testRun(t, 2)
	const frame = "data: {\"choices\":[],\"usage\":{}}\n\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, frame)
	}))
	defer srv.Close()
	tr := testTransport(t, openRun(t, run), srv, ItemIDKey)
	resp, err := testSend(t, tr, srv.URL, "item", `{"stream":true}`)
	if err != nil {
		t.Fatal(err)
	}
	raw := make([]byte, len(frame))
	if _, err := io.ReadFull(resp.Body, raw); err != nil || string(raw) != frame {
		t.Fatalf("stream = %q (%v)", raw, err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatal(err)
	}
	if rows := ledgerRows(t, run.Path, "settle"); len(rows) != 1 || rows[0]["settled_usd"] != nil {
		t.Fatalf("settle = %v", rows)
	}
	if calls := tr.TakeCalls("item"); len(calls) != 1 || calls[0].Usage != nil {
		t.Fatalf("calls = %+v", calls)
	}
}

func TestTransportSSECloseWithPartialExtraContent(t *testing.T) {
	run := testRun(t, 2)
	run.Family = "A"
	const frame = `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"extra_content":"{"}]}}]}` + "\n\n"
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, frame)
		w.(http.Flusher).Flush()
		<-release
	}))
	defer srv.Close()
	defer close(release)
	tr := testTransport(t, openRun(t, run), srv, AskIDKey)
	resp, err := testSend(t, tr, srv.URL, "ask", `{"stream":true}`)
	if err != nil {
		t.Fatal(err)
	}
	raw := make([]byte, len(frame))
	if _, err := io.ReadFull(resp.Body, raw); err != nil || string(raw) != frame {
		t.Fatalf("stream = %q (%v)", raw, err)
	}
	if rows := ledgerRows(t, run.Path, "settle"); len(rows) != 0 {
		t.Fatalf("settled before close: %v", rows)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatal(err)
	}
	rows := ledgerRows(t, run.Path, "settle")
	if len(rows) != 1 || rows[0]["id"] != "ask" || rows[0]["attempt"] != float64(1) {
		t.Fatalf("settle = %v", rows)
	}
	if usage, ok := rows[0]["settled_usd"]; !ok || usage != nil {
		t.Fatalf("want explicit null settlement: %v", rows[0])
	}
	if calls := tr.TakeCalls("ask"); len(calls) != 1 || calls[0].AskID != "ask" || !calls[0].Stream ||
		calls[0].Usage != nil || calls[0].ToolCalls != 1 || calls[0].SignatureIn {
		t.Fatalf("calls = %+v", calls)
	}
}
