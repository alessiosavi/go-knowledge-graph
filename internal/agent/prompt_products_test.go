package agent

import (
	"net/http"
	"os"
	"strings"
	"testing"
)

func TestAskDescribedProductPrompt(t *testing.T) {
	const rule = "- When the user describes a product by its use, feel, look or properties rather than by its name or code, make your first call search with kind=product, limit 10, and the user's message copied word for word as the query: do not shorten it to keywords or translate it, because meaning search matches whole descriptions. Then continue with your usual keyword searches."
	const candidatePrefix = "- A product hit matched by meaning is a candidate"
	golden, err := os.ReadFile("testdata/plan_system_prompt_c38db32.golden")
	if err != nil {
		t.Fatal(err)
	}
	wantPlan := strings.TrimSuffix(string(golden), "\n")
	wantSystem, _, ok := strings.Cut(wantPlan, "\n\nThis endpoint does not support tool calls.")
	if !ok {
		t.Fatal("golden plan prompt is missing its plan suffix")
	}

	for _, tc := range []struct {
		name   string
		search bool
	}{{"off", false}, {"on", true}} {
		t.Run(tc.name, func(t *testing.T) {
			stub := newLLMStub(t,
				stubResponse{status: http.StatusBadRequest, body: `{"error":"tools unsupported"}`},
				contentReply(`{"answer":"Here are the results."}`),
			)
			cfg := enabledCfg(stub.srv.URL)
			cfg.ProductText.Search = tc.search
			s := NewServer(cfg, nil, nil)
			if _, err := s.Ask(t.Context(), AskRequest{Question: "Cerco qualcosa che sia liscio e flessibile."}); err != nil {
				t.Fatal(err)
			}
			if len(stub.requests) != 2 {
				t.Fatalf("requests=%d, want one tool-calling turn and one plan turn", len(stub.requests))
			}
			for i, prompt := range []struct{ name, baseline string }{{"system", wantSystem}, {"plan", wantPlan}} {
				t.Run(prompt.name, func(t *testing.T) {
					msgs := stub.requests[i]["messages"].([]any)
					got := msgs[0].(map[string]any)["content"].(string)
					if !tc.search {
						if got != prompt.baseline {
							t.Error("flag-off prompt differs from the c38db32 prompt")
						}
						return
					}
					if count := strings.Count(got, rule); count != 1 {
						t.Errorf("description rule occurs %d times, want 1", count)
					}
					before, _, _ := strings.Cut(got, "\n"+rule+"\n")
					previous := before[strings.LastIndexByte(before, '\n')+1:]
					if !strings.HasPrefix(previous, candidatePrefix) {
						t.Error("description rule does not immediately follow the meaning-candidate bullet")
					}
					if strings.Replace(got, "\n"+rule+"\n", "\n", 1) != prompt.baseline {
						t.Error("flag-on prompt differs from c38db32 beyond the description rule")
					}
				})
			}
		})
	}
}
