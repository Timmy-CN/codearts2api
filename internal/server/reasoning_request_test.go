package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"codearts2api/internal/auth"
	"codearts2api/internal/pool"
	"codearts2api/internal/upstream"
)

// 请求里的 thinking / reasoning_effort 要真的到达上游（插件 0.3.3 口径）。
func TestChatReasoningOptionsReachUpstream(t *testing.T) {
	tests := []struct {
		name       string
		model      string
		extra      string
		wantEffort string
		wantThink  string
	}{
		{name: "thinking false is honored", model: "glm-5.2", extra: `"thinking":false`, wantThink: ""},
		{name: "unsupported level dropped", model: "glm-5.3-flash", extra: `"reasoning_effort":"medium"`, wantThink: ""},
		{name: "supported level kept", model: "glm-5.3-flash", extra: `"reasoning_effort":"high"`, wantEffort: "high", wantThink: ""},
		{name: "none disables thinking", model: "glm-5.2", extra: `"reasoning_effort":"none"`, wantThink: "disabled"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var got map[string]any
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				raw, _ := io.ReadAll(r.Body)
				_ = json.Unmarshal(raw, &got)
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"))
			}))
			defer backend.Close()

			auths := []*auth.Auth{auth.New("u1", "tester", "domain", "token", "ak", "sk",
				time.Now().Add(time.Hour).Format(time.RFC3339), "", "")}
			p, err := pool.New(auths, pool.Config{MaxConcurrent: 1, ErrThreshold: 3}, "")
			if err != nil {
				t.Fatal(err)
			}
			for _, acct := range p.Accounts() {
				acct.Client = upstream.NewWithChatEndpoint(time.Second, backend.URL)
			}
			h := NewHandler(Config{Pool: p, DefaultModel: tc.model, MaxRotate: 1, ErrThreshold: 3})

			payload := `{"model":"` + tc.model + `","stream":false,"messages":[{"role":"user","content":"hi"}]` +
				func() string {
					if tc.extra == "" {
						return "}"
					}
					return "," + tc.extra + "}"
				}()
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(payload)))
			if rec.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
			effort, _ := got["reasoning_effort"].(string)
			if effort != tc.wantEffort {
				t.Fatalf("upstream reasoning_effort = %q, want %q (body=%v)", effort, tc.wantEffort, got)
			}
			think := ""
			if v, ok := got["thinking"].(map[string]any); ok {
				think, _ = v["type"].(string)
			}
			if think != tc.wantThink {
				t.Fatalf("upstream thinking = %q, want %q (body=%v)", think, tc.wantThink, got)
			}
		})
	}
}
