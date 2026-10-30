package server

import (
	"testing"

	"codearts2api/internal/auth"
	"codearts2api/internal/pool"
	"codearts2api/internal/upstream"
)

// 插件 0.2.6：登录落盘时 IAM 内部 ID 形状不能成为账号名字；手机号读不到
// （这里 refresh_token 没有 principal_id，不会发 IAM 请求）也不影响登录。
func TestSaveLoginResultFiltersIAMIdentifiers(t *testing.T) {
	dir := t.TempDir()
	p, err := pool.New(nil, pool.Config{}, "")
	if err != nil {
		t.Fatal(err)
	}
	h := NewHandler(Config{AuthDir: dir, Pool: p})

	urn := "iam::d1:user:hw123456789012345"
	tok := &upstream.TokenResponse{
		UserID:       urn,
		UserName:     urn,
		RefreshToken: "opaque",
		Credentials:  upstream.Credentials{AccessKeyID: "ak", SecretAccessKey: "sk", SecurityToken: "st"},
	}
	if err := h.saveLoginResult(tok, "verifier"); err != nil {
		t.Fatal(err)
	}

	auths, err := auth.LoadDir(dir)
	if err != nil || len(auths) != 1 {
		t.Fatalf("LoadDir = %d auths, %v", len(auths), err)
	}
	if got := auths[0].UserName; got != "" {
		t.Errorf("stored name = %q, an IAM id must not be the account's name", got)
	}
	if got := auths[0].Phone(); got != "" {
		t.Errorf("phone = %q, no principal_id was in the token", got)
	}
	if tok.UserName != "" {
		t.Errorf("token name = %q, should be filtered too", tok.UserName)
	}
}
