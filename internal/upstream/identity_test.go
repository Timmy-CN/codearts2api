package upstream

import (
	"encoding/base64"
	"testing"
)

// IdentityFromRefreshToken 从 refresh_token JWT 的 user_profile 解账号身份
// （插件 accountIdentityOf 同口径）；STS 的 code/refresh 响应本身不带用户字段。
func TestIdentityFromRefreshTokenObjectProfile(t *testing.T) {
	payload := `{"user_profile":{"principal_urn":"iam:codearts:user:zhangsan","user_name":"张三","domain_name":"d1"},"sub":"sub-1"}`
	tok := "h." + base64.RawURLEncoding.EncodeToString([]byte(payload)) + ".s"
	id := IdentityFromRefreshToken(tok)
	if id.ID != "iam:codearts:user:zhangsan" || id.Name != "张三" {
		t.Fatalf("identity = %+v", id)
	}
}

// user_profile 是再 base64 一层的 JSON 字符串（双重编码）也能解。
func TestIdentityFromRefreshTokenDoubleEncodedProfile(t *testing.T) {
	inner := []byte(`{"account_id":"1001","account_name":"acct-a","domain_name":"dom"}`)
	outer := `{"user_profile":"` + base64.StdEncoding.EncodeToString(inner) + `"}`
	tok := "h." + base64.RawURLEncoding.EncodeToString([]byte(outer)) + ".s"
	id := IdentityFromRefreshToken(tok)
	if id.ID != "acct-a" || id.Name != "acct-a" {
		t.Fatalf("identity = %+v, 期望 id=acct-a name=acct-a（account_name 优先于 account_id）", id)
	}
}

// 没有 user_profile / 非 JWT / 非法 base64 → 零值（调用方走 STS 查询兜底）。
func TestIdentityFromRefreshTokenMissing(t *testing.T) {
	for name, tok := range map[string]string{
		"无 user_profile": "h." + base64.RawURLEncoding.EncodeToString([]byte(`{"sub":"x"}`)) + ".s",
		"非 JWT":          "not-a-jwt",
		"非法 base64":      "h.!!!!.s",
	} {
		if id := IdentityFromRefreshToken(tok); id.ID != "" || id.Name != "" {
			t.Fatalf("%s: identity = %+v, 期望零值", name, id)
		}
	}
}
