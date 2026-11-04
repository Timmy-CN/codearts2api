package upstream

import "testing"

func TestReasoningLevelsLookupIsCaseInsensitive(t *testing.T) {
	if got := ReasoningLevels("GLM-5.2"); len(got) != 6 {
		t.Fatalf("GLM-5.2 levels = %v", got)
	}
	if got := ReasoningLevels("glm-5.3-flash"); len(got) != 4 {
		t.Fatalf("glm-5.3-flash levels = %v", got)
	}
	if got := ReasoningLevels("kimi-k3.1-pro"); len(got) != 5 {
		t.Fatalf("kimi-k3.1-pro levels = %v", got)
	}
	if got := ReasoningLevels("unknown-model"); got != nil {
		t.Fatalf("unknown model should have no measured levels: %v", got)
	}
}

// 请求体里的 reasoning_effort 按模型表处理（插件 0.3.3/0.3.4）。
func TestChatBodyReasoningEffort(t *testing.T) {
	tests := []struct {
		name       string
		model      string
		effort     string
		thinking   *bool
		wantEffort string
		wantThink  string // "" = 没有 thinking 字段
	}{
		{name: "unsupported level dropped", model: "glm-5.3-flash", effort: "medium", wantEffort: "", wantThink: ""},
		{name: "supported level kept", model: "glm-5.3-flash", effort: "high", wantEffort: "high", wantThink: ""},
		{name: "uppercase model supported", model: "GLM-5.2", effort: "xhigh", wantEffort: "xhigh", wantThink: ""},
		{name: "none disables thinking", model: "glm-5.2", effort: "none", wantEffort: "", wantThink: "disabled"},
		{name: "off disables thinking", model: "kimi-k3", effort: "off", wantEffort: "", wantThink: "disabled"},
		{name: "unknown model passes level", model: "unknown-model", effort: "high", wantEffort: "high", wantThink: ""},
		{name: "no effort injects thinking", model: "glm-5.2", wantEffort: "", wantThink: "enabled"},
		{name: "explicit thinking false", model: "glm-5.2", thinking: boolPtr(false), wantEffort: "", wantThink: ""},
		{name: "caller thinking wins over none", model: "glm-5.2", effort: "none", thinking: boolPtr(true), wantEffort: "", wantThink: "enabled"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			body := chatBodyV2("", []ChatMessage{{Role: "user", Content: "hi"}}, tc.model, ChatOptions{
				ReasoningEffort: tc.effort,
				Thinking:        tc.thinking,
			})
			if got, _ := body["reasoning_effort"].(string); got != tc.wantEffort {
				t.Fatalf("reasoning_effort = %q, want %q (body=%v)", got, tc.wantEffort, body)
			}
			think := ""
			if v, ok := body["thinking"].(map[string]any); ok {
				think, _ = v["type"].(string)
			}
			if think != tc.wantThink {
				t.Fatalf("thinking = %q, want %q (body=%v)", think, tc.wantThink, body)
			}
		})
	}
}

func boolPtr(v bool) *bool { return &v }
