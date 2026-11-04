// 模型推理等级（插件 codearts 0.3.3/0.3.4 口径）。
//
// 网关按模型校验 reasoning_effort：模型不接受的等级直接 400
// （InferHub.ModelArts.81001.400），所以调用方点名的等级要么原样发，要么摘掉，
// 不能留给上游把整个请求打回。各模型接受的等级是 2026-11-04 实测表。
package upstream

import "strings"

// reasoningLevelsByModel 是各模型接受的等级；key 一律小写（目录里 GLM-5.2
// 与网关的 glm-5.2 是同一个模型）。
var reasoningLevelsByModel = map[string][]string{
	"deepseek-v4.1-flash-1017": {"none", "low", "medium", "high", "xhigh", "max"},
	"deepseek-v4.1-flash":      {"none", "low", "medium", "high", "xhigh", "max"},
	"glm-5.3-flash":            {"none", "low", "high", "max"},
	"glm-5.3":                  {"none", "low", "medium", "high", "xhigh", "max"},
	"glm-5.2":                  {"none", "low", "medium", "high", "xhigh", "max"},
	"glm-5.2-sft-harmony":      {"none", "low", "medium", "high", "xhigh", "max"},
	"openpangu-2.0-pro":        {"none", "low", "medium", "high", "xhigh", "max"},
	"openpangu-2.0-flash":      {"none", "low", "medium", "high", "xhigh", "max"},
	"speed":                    {"none", "low", "medium", "high", "xhigh", "max"},
	// 两个 Kimi 没有 none：给了也照样思考（实测 reasoning 82/290 字符），
	// 所以不宣称一个它做不到的「关闭思考」等级。
	"kimi-k3":       {"low", "medium", "high", "xhigh", "max"},
	"kimi-k3.1-pro": {"low", "medium", "high", "xhigh", "max"},
}

// noThinkingEfforts 是「关闭思考」的几种写法；网关自己的词是 none。
var noThinkingEfforts = map[string]bool{"none": true, "off": true, "disabled": true}

// ReasoningLevels 返回模型接受的等级；未知模型返回 nil（等级原样透传）。
func ReasoningLevels(model string) []string {
	levels, ok := reasoningLevelsByModel[strings.ToLower(strings.TrimSpace(model))]
	if !ok {
		return nil
	}
	return levels
}

// IsNoThinkingEffort 判断该等级是否表示「关闭思考」。
func IsNoThinkingEffort(level string) bool {
	return noThinkingEfforts[strings.ToLower(strings.TrimSpace(level))]
}

func levelIn(levels []string, level string) bool {
	for _, l := range levels {
		if l == level {
			return true
		}
	}
	return false
}
