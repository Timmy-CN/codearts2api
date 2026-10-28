package server

// toolnames.go 工具名超限处理（插件 0.2.2 口径，#432）：CodeArts 网关拒绝
// 超过 64 字符的函数名（InferHub.001001017.413 "Function name [...] length
// over limit, maximum is 64"），而 agent 的 MCP 工具名常超限（如
// mcp__plugin_android-emulator_android-emulator__android_discover_project，
// 71 字符）。
//
// 口径（插件 FlatName）：前 55 字符 + `_` + SHA-256 前 8 位 hex（共 64）。
// 同一名字每次请求切法相同——历史里的调用继续对得上同名工具；前缀相同的
// 两个名字靠 hash 区分。撞上已有名时加盐重切，映射永远一一对应。
// 应答里的 tool_calls 再把发送名还原成 agent 自己的（restoreToolNames /
// restoreStreamToolNames）。

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
)

const (
	toolNameLimit = 64
	toolNameKeep  = 55
)

// cutToolName 截一个名字。
func cutToolName(name string, salt int) string {
	head := []rune(name)
	if len(head) > toolNameKeep {
		head = head[:toolNameKeep]
		// 不留下半个代理对（高代理结尾再退一位）
		for len(head) > 0 && head[len(head)-1] >= 0xD800 && head[len(head)-1] <= 0xDBFF {
			head = head[:len(head)-1]
		}
	}
	h := sha256.New()
	fmt.Fprintf(h, "%s", name)
	if salt > 0 {
		fmt.Fprintf(h, "\x00%d", salt)
	}
	return string(head) + "_" + hex.EncodeToString(h.Sum(nil))[:8]
}

// shortenToolNames 把 req 里所有超限的工具名原位切短（请求侧所有点名位置：
// tools[].function.name、历史 assistant.tool_calls[].function.name、tool
// 消息的 name）。返回 发送名 → agent 原名 的映射；没有超限名时返回 nil。
func shortenToolNames(req *chatRequest) map[string]string {
	type named struct {
		get func() string
		set func(string)
	}
	var slots []named
	for _, t := range req.Tools {
		if fn, ok := t["function"].(map[string]any); ok {
			if v, ok := fn["name"].(string); ok && v != "" {
				f := fn
				slots = append(slots, named{
					get: func() string { return f["name"].(string) },
					set: func(v string) { f["name"] = v },
				})
			}
		}
	}
	for i := range req.Messages {
		m := &req.Messages[i]
		for j := range m.ToolCalls {
			c := &m.ToolCalls[j]
			if c.Name != "" {
				slots = append(slots, named{
					get: func() string { return c.Name },
					set: func(v string) { c.Name = v },
				})
			}
		}
		if (m.Role == "tool" || m.Role == "function") && m.Name != "" {
			nm := m
			slots = append(slots, named{
				get: func() string { return nm.Name },
				set: func(v string) { nm.Name = v },
			})
		}
	}
	if len(slots) == 0 {
		return nil
	}

	anyLong := false
	for _, s := range slots {
		if len([]rune(s.get())) > toolNameLimit {
			anyLong = true
			break
		}
	}
	if !anyLong {
		return nil
	}

	taken := map[string]bool{} // 已占用的名字（原短名 + 已切出的名）
	uniq := map[string]bool{}
	for _, s := range slots {
		n := s.get()
		uniq[n] = true
		if len([]rune(n)) <= toolNameLimit {
			taken[n] = true
		}
	}
	names := map[string]string{} // 发送名 → agent 原名
	short := map[string]string{} // agent 原名 → 发送名

	// 固定顺序处理：同一批名字每次请求切法一致。
	var long []string
	for n := range uniq {
		if len([]rune(n)) > toolNameLimit {
			long = append(long, n)
		}
	}
	sort.Strings(long)

	for _, name := range long {
		cut := cutToolName(name, 0)
		for salt := 1; taken[cut]; salt++ {
			cut = cutToolName(name, salt)
		}
		short[name] = cut
		names[cut] = name
		taken[cut] = true
	}
	for _, s := range slots {
		if cut, ok := short[s.get()]; ok {
			s.set(cut)
		}
	}
	return names
}

// restoreToolNames 把非流式应答的 tool_calls 发送名就地还原成 agent 原名。
func restoreToolNames(calls []openAIToolCall, names map[string]string) {
	if names == nil {
		return
	}
	for i := range calls {
		if orig, ok := names[calls[i].Name]; ok {
			calls[i].Name = orig
		}
	}
}
