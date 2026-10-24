package upstream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const testCredToken = "st"

func testCred() SignCredential {
	return SignCredential{AccessKeyID: "ak", SecretAccessKey: "sk", SecurityToken: testCredToken}
}

func sseOK() string {
	return "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"
}

// 网关排队：429 + queue-status:queued + trace-id → 轮询 trace-queues 到
// promoted → 重发同一请求并成功。
func TestChatWaitsInGatewayQueue(t *testing.T) {
	var posts, polls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == EpChatV2:
			if posts.Add(1) == 1 {
				w.Header().Set("queue-status", "queued")
				w.Header().Set("trace-id", "T1")
				w.Header().Set("retry-after", "0")
				w.WriteHeader(http.StatusTooManyRequests)
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, sseOK())
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, EpTraceQueues+"/"):
			if polls.Add(1) == 1 {
				_, _ = io.WriteString(w, `{"status":"queued","retry_after":0}`)
				return
			}
			_, _ = io.WriteString(w, `{"status":"promoted"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c := NewWithChatEndpoint(5*time.Second, srv.URL+EpChatV2)
	c.snapBase = srv.URL
	rc, err := c.ChatStreamWithOptions(context.Background(), "chat-1", nil, "", testCred(), "user", "GLM-5.2", ChatOptions{}, false, "")
	if err != nil {
		t.Fatalf("排队后重发应成功: %v", err)
	}
	rc.Close()
	if posts.Load() != 2 {
		t.Fatalf("chat 次数 = %d, 期望 2", posts.Load())
	}
	if polls.Load() != 2 {
		t.Fatalf("轮询次数 = %d, 期望 2", polls.Load())
	}
}

// 预算内等不到 promoted：DELETE 退出队列并把 429 交还调用方。
func TestChatQueueGivesUpAndDeletes(t *testing.T) {
	var deletes atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == EpChatV2:
			w.Header().Set("queue-status", "queued")
			w.Header().Set("trace-id", "T2")
			w.Header().Set("retry-after", "0")
			w.WriteHeader(http.StatusTooManyRequests)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, EpTraceQueues+"/"):
			_, _ = io.WriteString(w, `{"status":"queued","retry_after":30}`)
		case r.Method == http.MethodDelete:
			deletes.Add(1)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c := NewWithChatEndpoint(5*time.Second, srv.URL+EpChatV2)
	c.snapBase = srv.URL
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	_, err := c.ChatStreamWithOptions(ctx, "chat-2", nil, "", testCred(), "user", "GLM-5.2", ChatOptions{}, false, "")
	var apiErr *ApiError
	if err == nil || !strings.Contains(err.Error(), "排队等待超时") {
		t.Fatalf("应回 429 排队超时: %v", err)
	}
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusTooManyRequests {
		t.Fatalf("应保留 429 语义: %v", err)
	}
	if deletes.Load() != 1 {
		t.Fatalf("退出时应 DELETE 排队 trace, 实际 %d 次", deletes.Load())
	}
}

// 签名请求不跟随重定向：3xx 一律转为 502 错误（凭据不外泄）。
func TestChatRedirectRejected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "https://evil.example.com/collect")
		w.WriteHeader(http.StatusFound)
	}))
	defer srv.Close()

	c := NewWithChatEndpoint(5*time.Second, srv.URL+EpChatV2)
	_, err := c.ChatStreamWithOptions(context.Background(), "chat-3", nil, "", testCred(), "user", "GLM-5.2", ChatOptions{}, false, "")
	if err == nil || !strings.Contains(err.Error(), "unexpected redirect 302") {
		t.Fatalf("重定向应被拒绝: %v", err)
	}
}

// thinking 注入与 max_tokens 钳制：未显式指定时注入 enabled；显式 false 不注入；
// 超上限的 max_tokens 钳到 65536。
func TestChatBodyThinkingAndMaxTokensClamp(t *testing.T) {
	big := 999999
	off := false
	on := true
	cases := []struct {
		name          string
		opts          ChatOptions
		wantThinking  bool
		wantMaxTokens int64
	}{
		{"默认注入+钳制", ChatOptions{MaxTokens: &big}, true, MaxOutputTokens},
		{"显式关闭", ChatOptions{Thinking: &off}, false, 0},
		{"显式开启", ChatOptions{Thinking: &on}, true, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var seen map[string]any
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_ = json.NewDecoder(r.Body).Decode(&seen)
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, sseOK())
			}))
			defer srv.Close()
			c := NewWithChatEndpoint(5*time.Second, srv.URL+EpChatV2)
			rc, err := c.ChatStreamWithOptions(context.Background(), "chat-4", nil, "", testCred(), "user", "GLM-5.2", tc.opts, false, "")
			if err != nil {
				t.Fatalf("chat: %v", err)
			}
			rc.Close()
			_, hasThinking := seen["thinking"]
			if hasThinking != tc.wantThinking {
				t.Fatalf("thinking 存在 = %v, 期望 %v", hasThinking, tc.wantThinking)
			}
			if tc.wantMaxTokens != 0 {
				if mt, _ := seen["max_tokens"].(float64); int64(mt) != tc.wantMaxTokens {
					t.Fatalf("max_tokens = %v, 期望 %d", seen["max_tokens"], tc.wantMaxTokens)
				}
			}
		})
	}
}

// 同账号（同 sessionKey）的请求共用稳定 x-ot-session-id（网关按它数并发会话），
// trace/span 每次随机；sessionKey 为空时不带会话头（调试工具路径）。
func TestChatStableSessionHeaders(t *testing.T) {
	var mu sync.Mutex
	var sessions []string
	var traces []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		sessions = append(sessions, r.Header.Get("x-ot-session-id"))
		traces = append(traces, r.Header.Get("x-ot-trace-id"))
		mu.Unlock()
		if r.Header.Get("x-ot-session-id") == "" {
			// sessionKey 为空（调试路径）：不带任何会话头。
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, sseOK())
			return
		}
		if r.Header.Get("user-session-id") != r.Header.Get("x-ot-session-id") {
			t.Errorf("user-session-id 应与 x-ot-session-id 同值")
		}
		if r.Header.Get("x-ot-span-id") == "" {
			t.Errorf("x-ot-span-id 缺失")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, sseOK())
	}))
	defer srv.Close()

	c := NewWithChatEndpoint(5*time.Second, srv.URL+EpChatV2)
	assertSessions := func(want []string) {
		t.Helper()
		mu.Lock()
		defer mu.Unlock()
		if len(sessions) != len(want) {
			t.Fatalf("会话头序列 = %v, 期望 %v", sessions, want)
		}
		for i := range want {
			if sessions[i] != want[i] {
				t.Fatalf("会话头序列 = %v, 期望 %v", sessions, want)
			}
		}
	}
	for i := range 2 {
		chatID := fmt.Sprintf("chat-%d", i) // 不同 chat_id，同一账号
		rc, err := c.ChatStreamWithOptions(context.Background(), chatID, nil, "", testCred(), "user", "GLM-5.2", ChatOptions{}, false, "acct-u1")
		if err != nil {
			t.Fatalf("chat %d: %v", i, err)
		}
		rc.Close()
	}
	stable := stableSessionID("acct-u1")
	assertSessions([]string{stable, stable})
	mu.Lock()
	if traces[0] == "" || traces[0] == traces[1] {
		t.Fatalf("x-ot-trace-id 应存在且每次不同: %v", traces)
	}
	mu.Unlock()

	// sessionKey 为空（SendChatV2/调试路径）→ 不带会话头。
	rc, err := c.ChatStreamWithOptions(context.Background(), "chat-9", nil, "", testCred(), "user", "GLM-5.2", ChatOptions{}, false, "")
	if err != nil {
		t.Fatalf("chat-9: %v", err)
	}
	rc.Close()
	assertSessions([]string{stable, stable, ""})
}
