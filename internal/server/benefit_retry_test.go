package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"codearts2api/internal/auth"
	"codearts2api/internal/pool"
	"codearts2api/internal/upstream"
)

type benefitClientFunc func(cred upstream.SignCredential) error

func (f benefitClientFunc) ClaimBenefit(cred upstream.SignCredential) error { return f(cred) }

// 福利模型报「benefit not found」（当天未领取）时：先补领一次再重发同一账号，
// 领取后上游正常应答，客户端拿到 200。
func TestChatBenefitErrorClaimsThenRetries(t *testing.T) {
	var attempts, claims atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := attempts.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		if n == 1 {
			_, _ = w.Write([]byte("data: {\"error_code\":\"InferHub.4004.200\",\"error_msg\":\"benefit not found\"}\n\n"))
			return
		}
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"))
	}))
	defer backend.Close()

	claims.Store(0)
	benefitClient := benefitClientFunc(func(cred upstream.SignCredential) error {
		claims.Add(1)
		return nil
	})
	auths := []*auth.Auth{
		auth.New("u1", "tester", "domain", "token", "ak", "sk", time.Now().Add(time.Hour).Format(time.RFC3339), "", ""),
	}
	p, err := pool.New(auths, pool.Config{MaxConcurrent: 1, ErrThreshold: 3, BenefitClient: benefitClient}, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, acct := range p.Accounts() {
		acct.Client = upstream.NewWithChatEndpoint(time.Second, backend.URL)
	}
	h := NewHandler(Config{Pool: p, DefaultModel: "glm-5.3-flash", MaxRotate: 1, ErrThreshold: 3})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.3-flash","stream":false,"messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("领取后重试应成功: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if claims.Load() != 1 {
		t.Fatalf("补领次数 = %d, 期望 1", claims.Load())
	}
	if attempts.Load() != 2 {
		t.Fatalf("上游尝试次数 = %d, 期望 2（首失败 + 补领后重发）", attempts.Load())
	}
}

// 注入的 thinking 被模型拒绝（400 文案点名 thinking）时：摘除后同账号重发一次。
// 其他参数错误（文案不含 thinking）不重试，直接 400。
func TestChatThinkingRejectedStripsAndRetries(t *testing.T) {
	cases := []struct {
		name         string
		thinkingErr  bool // 桩对带 thinking 的请求返回的错误是否点名 thinking
		wantStatus   int
		wantUpstream int
	}{
		{"thinking 被拒 → 摘除重发成功", true, http.StatusOK, 2},
		{"其余参数错误 → 不重试直接 400", false, http.StatusBadRequest, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var attempts atomic.Int32
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				attempts.Add(1)
				var body map[string]any
				_ = json.NewDecoder(r.Body).Decode(&body)
				_, hasThinking := body["thinking"]
				w.Header().Set("Content-Type", "text/event-stream")
				if hasThinking {
					if tc.thinkingErr {
						_, _ = w.Write([]byte("data: {\"error_code\":\"InferHub.001001005.400\",\"error_msg\":\"thinking is not supported\"}\n\n"))
					} else {
						_, _ = w.Write([]byte("data: {\"error_code\":\"InferHub.001001005.400\",\"error_msg\":\"The request param is invalid\"}\n\n"))
					}
					return
				}
				if tc.thinkingErr {
					_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"))
					return
				}
				_, _ = w.Write([]byte("data: {\"error_code\":\"InferHub.001001005.400\",\"error_msg\":\"The request param is invalid\"}\n\n"))
			}))
			defer backend.Close()

			auths := []*auth.Auth{
				auth.New("u1", "tester", "domain", "token", "ak", "sk", time.Now().Add(time.Hour).Format(time.RFC3339), "", ""),
			}
			p, err := pool.New(auths, pool.Config{MaxConcurrent: 1, ErrThreshold: 3}, "")
			if err != nil {
				t.Fatal(err)
			}
			for _, acct := range p.Accounts() {
				acct.Client = upstream.NewWithChatEndpoint(time.Second, backend.URL)
			}
			h := NewHandler(Config{Pool: p, DefaultModel: "glm-5.2", MaxRotate: 1, ErrThreshold: 3})

			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
				strings.NewReader(`{"model":"glm-5.2","stream":false,"messages":[{"role":"user","content":"hi"}]}`)))
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, 期望 %d, body=%s", rec.Code, tc.wantStatus, rec.Body.String())
			}
			if attempts.Load() != int32(tc.wantUpstream) {
				t.Fatalf("上游尝试次数 = %d, 期望 %d", attempts.Load(), tc.wantUpstream)
			}
		})
	}
}
