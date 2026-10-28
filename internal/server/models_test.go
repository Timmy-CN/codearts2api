package server

import (
	"crypto/sha256"
	"encoding/hex"
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

// 混合账号池：/v1/models 展示的是并集，聊天必须先挑目录里真有该模型的账号。
// 否则没这个模型的账号会先吃一次 400，白耗一次 MaxRotate（只有 3 次），
// 还会给健康账号记错误、累计到阈值冷却 10 分钟。
func TestPickAccountPrefersAccountWithModel(t *testing.T) {
	p, err := pool.New([]*auth.Auth{
		{UserID: "acct-a", UserName: "a"},
		{UserID: "acct-b", UserName: "b"},
		{UserID: "acct-c", UserName: "c"},
		{UserID: "acct-d", UserName: "d"}, // 未发现目录
	}, pool.Config{}, "")
	if err != nil {
		t.Fatal(err)
	}
	h := NewHandler(Config{Pool: p, Upstream: upstream.New(time.Second)})

	upstream.SetAccountModels("acct-a", []upstream.ModelInfo{{ID: "GLM-5.2"}})
	upstream.SetAccountModels("acct-b", []upstream.ModelInfo{{ID: "GLM-5.2"}})
	upstream.SetAccountModels("acct-c", []upstream.ModelInfo{
		{ID: "GLM-5.2"}, {ID: "deepseek-v4-pro-0813", Benefit: true},
	})

	got := h.pickAccount(map[string]bool{}, "deepseek-v4-pro-0813")
	if got == nil || got.Name != "acct-c" {
		t.Fatalf("应挑目录里有该模型的账号，got=%v", got)
	}
	// 有能力小写别名也要命中。
	got = h.pickAccount(map[string]bool{}, "glm-5.2")
	if got == nil || got.Name != "acct-a" {
		t.Fatalf("小写别名应命中第一个有能力账号，got=%v", got)
	}
	// 有能力但已被 tried 排除 → 退回普通轮转，不能返回同一账号。
	got = h.pickAccount(map[string]bool{"acct-c": true}, "deepseek-v4-pro-0813")
	if got == nil || got.Name == "acct-c" {
		t.Fatalf("被排除的账号不应再被选中，got=%v", got)
	}
	// 谁都没有这个模型 → 退回普通轮转（目录只是参考，不能凭它拒请求）。
	if got = h.pickAccount(map[string]bool{}, "unknown-model-xyz"); got == nil {
		t.Fatal("应退回普通轮转而不是拒绝请求")
	}

	// 粘性账号不能服务该模型时必须换号。
	if h.accountCanServe(p.Get("acct-a"), "deepseek-v4-pro-0813") {
		t.Error("acct-a 目录里没有该福利模型")
	}
	if !h.accountCanServe(p.Get("acct-c"), "DEEPSEEK-V4-PRO-0813") {
		t.Error("有该模型的账号应通过能力检查（大小写不敏感）")
	}
	// 目录未知的账号乐观放行，交给上游判定；nil 也不能崩。
	if !h.accountCanServe(p.Get("acct-d"), "deepseek-v4-pro-0813") {
		t.Error("目录未知时应乐观放行")
	}
	if !h.accountCanServe(nil, "deepseek-v4-pro-0813") {
		t.Error("账号不存在时应乐观放行")
	}
}

func TestModelEntriesAliasesStaticAndBenefit(t *testing.T) {
	entries := modelEntries([]upstream.ModelInfo{
		{ID: "GLM-5.2", ContextWindow: 202752, MaxTokens: 8192},
		{ID: "deepseek-v4-pro-0813", ContextWindow: 1048576, Benefit: true},
	})
	byID := map[string]map[string]any{}
	for _, e := range entries {
		id, _ := e["id"].(string)
		if byID[id] != nil {
			t.Fatalf("/v1/models 出现重复 ID: %s", id)
		}
		byID[id] = e
	}

	// 精确 ID 与小写别名都要在（老客户端习惯用小写）。
	for _, want := range []string{"GLM-5.2", "glm-5.2", "deepseek-v4-pro-0813"} {
		if byID[want] == nil {
			t.Errorf("缺少模型条目 %q，got=%v", want, keysOf(byID))
		}
	}
	if byID["GLM-5.2"]["context_length"] != int64(202752) {
		t.Errorf("context_length 应取自上游目录，got=%v", byID["GLM-5.2"])
	}
	if byID["GLM-5.2"]["max_output_tokens"] != int64(8192) {
		t.Errorf("max_output_tokens 应取自上游目录，got=%v", byID["GLM-5.2"])
	}
	if byID["deepseek-v4-pro-0813"]["benefit"] != true {
		t.Error("福利模型应在 /v1/models 标记 benefit")
	}

	// 福利模型不再进静态表（按批次轮换，静态固化只会过期——2026-10-25 实测
	// 旧批次 claim 已报 4000 invalid channel）：动态目录里没有就不出现。
	for _, gone := range []string{"glm-5.3-flash", "deepseek-v4-flash-0731"} {
		if byID[gone] != nil {
			t.Errorf("过期福利模型 %q 不应再由静态表兜底", gone)
		}
	}
	// -vl 视觉模型不进列表（与官方 IDE/插件一致，上下文小、无 tool call）。
	for _, hidden := range []string{"Qwen3-VL-235B", "qwen3-vl-235b"} {
		if byID[hidden] != nil {
			t.Errorf("视觉模型 %q 应从目录隐藏", hidden)
		}
	}

	// 动态目录整体失败（无账号可用）时仍返回完整静态表 + 别名。
	fallback := modelEntries(nil)
	if len(fallback) < len(staticModels) {
		t.Fatalf("静态兜底条目过少: %d", len(fallback))
	}
	seen := map[string]bool{}
	for _, e := range fallback {
		id, _ := e["id"].(string)
		if seen[id] {
			t.Fatalf("静态兜底出现重复 ID: %s", id)
		}
		seen[id] = true
	}
	if !seen["GLM-5.2"] || !seen["glm-5.2"] {
		t.Errorf("静态兜底也要给大写 ID 补小写别名，got=%v", keysOf(seen))
	}
}

func keysOf[T any](m map[string]T) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestNotMultimodalIs400 插件 0.2.4：图片发给纯文本模型（InferHub.001001020.406）
// 归类为 400 请求错误——不重试、不换号、不冷却账号。
func TestNotMultimodalIs400(t *testing.T) {
	var chatCalls atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// NewWithChatEndpoint(up.URL) 原样 POST 到 /：聊天即此分支
		if r.Method == http.MethodPost && (r.URL.Path == "/" || strings.HasSuffix(r.URL.Path, "/chat/completions")) {
			chatCalls.Add(1)
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("data: {\"error_code\":\"InferHub.001001020.406\",\"error_msg\":\"The request model is not multimodal\"}\n\n"))
			return
		}
		if strings.HasSuffix(r.URL.Path, "/get_detail_param") || strings.HasSuffix(r.URL.Path, "/batch_get_detail_param") {
			w.Write([]byte(`{}`))
			return
		}
		w.Write([]byte(`{}`))
	}))
	defer up.Close()

	auths := []*auth.Auth{
		auth.New("u1", "tester", "domain", "token", "ak", "sk", time.Now().Add(time.Hour).Format(time.RFC3339), "", ""),
		auth.New("u2", "tester2", "domain", "token", "ak", "sk", time.Now().Add(time.Hour).Format(time.RFC3339), "", ""),
	}
	p, err := pool.New(auths, pool.Config{MaxConcurrent: 1}, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, acct := range p.Accounts() {
		acct.Client = upstream.NewWithChatEndpoint(time.Second, up.URL)
	}
	h := NewHandler(Config{Pool: p, Upstream: upstream.New(time.Second), DefaultModel: "GLM-5.2", MaxRotate: 3})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"GLM-5.2","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,x"}}]}]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("not-multimodal 应 400, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "takes no images") {
		t.Fatalf("应透出图片拒收文案: %s", rec.Body.String())
	}
	if n := chatCalls.Load(); n != 1 {
		t.Fatalf("不换号不重试, chat calls=%d want 1", n)
	}
	// 账号不得被冷却/记错误（请求方的问题）：模型也不得被标为不可用
	if h.modelKnownUnusable("GLM-5.2") {
		t.Error("not-multimodal 不得把模型标记为不可用")
	}
	if h.modelKnownUnusable("GLM-5.2") {
		t.Error("not-multimodal 不得把模型标记为不可用")
	}
}

// TestToolNameOverLimitShortenedAndRestored 插件 0.2.2（#432）：超 64 字符的
// 工具名按前 55+`_`+8 位 hash 切短发上游；应答里的调用还原成 agent 原名。
func TestToolNameOverLimitShortenedAndRestored(t *testing.T) {
	const longName = "mcp__plugin_android-emulator_android-emulator__android_discover_project" // 71 字符
	// 真实切法（cutToolName 口径）：前 55 rune + "_" + sha256(longName) 前 8 hex
	cut := longName[:55] + "_" + func() string {
		sum := sha256.Sum256([]byte(longName))
		return hex.EncodeToString(sum[:])[:8]
	}()
	var gotTools []map[string]any
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && (r.URL.Path == "/" || strings.HasSuffix(r.URL.Path, "/chat/completions")) {
			var body struct {
				Tools []struct {
					Function struct {
						Name string `json:"name"`
					} `json:"function"`
				} `json:"tools"`
				Messages []struct {
					Role      string `json:"role"`
					ToolCalls []struct {
						Function struct {
							Name string `json:"name"`
						} `json:"function"`
					} `json:"tool_calls"`
					Name string `json:"name"`
				} `json:"messages"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			for _, tl := range body.Tools {
				gotTools = append(gotTools, map[string]any{"name": tl.Function.Name})
			}
			// 历史里的 assistant.tool_calls 名字应与本次 tools 一致（同切法）
			w.Header().Set("Content-Type", "text/event-stream")
			// 上游按切短名回调用；客户端应看到原长名
			_, _ = w.Write([]byte(`data: {"choices":[{"delta":{"tool_calls":[{"id":"call_1","type":"function","function":{"name":"` + cut + `","arguments":"{}"}}]}}]}` + "\n\n"))
			_, _ = w.Write([]byte(`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}` + "\n\n"))
			_, _ = w.Write([]byte("data: [DONE]\n\n"))
			return
		}
		w.Write([]byte(`{}`))
	}))
	defer up.Close()

	auths := []*auth.Auth{
		auth.New("u1", "tester", "domain", "token", "ak", "sk", time.Now().Add(time.Hour).Format(time.RFC3339), "", ""),
	}
	p, err := pool.New(auths, pool.Config{MaxConcurrent: 1}, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, acct := range p.Accounts() {
		acct.Client = upstream.NewWithChatEndpoint(time.Second, up.URL)
	}
	h := NewHandler(Config{Pool: p, Upstream: upstream.NewWithChatEndpoint(time.Second, up.URL), DefaultModel: "GLM-5.2", MaxRotate: 1})

	// 第二轮对话：历史里带着原长名的 tool_calls（客户端视角名字从未变过）
	reqBody := `{"model":"GLM-5.2","messages":[
		{"role":"user","content":"list projects"},
		{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"` + longName + `","arguments":"{}"}}]},
		{"role":"tool","tool_call_id":"call_1","name":"` + longName + `","content":"[]"}
	],"tools":[{"type":"function","function":{"name":"` + longName + `","parameters":{"type":"object"}}}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(reqBody))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if len(gotTools) != 1 {
		t.Fatalf("tools=%v", gotTools)
	}
	sent := gotTools[0]["name"].(string)
	if len([]rune(sent)) > 64 {
		t.Fatalf("发送名超限: %q (%d)", sent, len([]rune(sent)))
	}
	if !strings.HasPrefix(sent, longName[:55]+"_") {
		t.Fatalf("发送名应为前 55 字符 + hash, got %q", sent)
	}
	if sent == longName {
		t.Fatal("长名应被切短")
	}

	// 客户端应答里的 tool_calls 名字必须是原长名
	var out struct {
		Choices []struct {
			Message struct {
				ToolCalls []struct {
					Function struct {
						Name string `json:"name"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Choices) == 0 || len(out.Choices[0].Message.ToolCalls) == 0 {
		t.Fatalf("应答无 tool_calls: %s", rec.Body.String())
	}
	if got := out.Choices[0].Message.ToolCalls[0].Function.Name; got != longName {
		t.Fatalf("应答名字应还原为原长名, got %q", got)
	}
}
