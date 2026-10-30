package pool

import (
	"testing"
	"time"

	"codearts2api/internal/auth"
)

// 面板昵称：脱敏手机号优先；IAM 内部 ID 形状的名字不外露（插件 0.2.5/0.2.6）。
func TestNicknamePrefersPhoneAndDropsIdentifiers(t *testing.T) {
	urn := "iam::d1:user:hw123456789012345"

	withPhone := auth.New("u1", urn, "dom", "tok", "ak", "sk", time.Now().Add(time.Hour).Format(time.RFC3339), "", "")
	withPhone.SetUserPhone("138****8000")

	idOnly := auth.New("u2", urn, "dom", "tok", "ak", "sk", time.Now().Add(time.Hour).Format(time.RFC3339), "", "")

	named := auth.New("u3", "张三", "dom", "tok", "ak", "sk", time.Now().Add(time.Hour).Format(time.RFC3339), "", "")

	p, err := New(nil, Config{}, "")
	if err != nil {
		t.Fatal(err)
	}
	p.AddAccount(withPhone)
	p.AddAccount(idOnly)
	p.AddAccount(named)

	got := map[string]string{}
	for _, row := range p.List() {
		got[row["uid"].(string)] = row["nickname"].(string)
	}
	if got["u1"] != "138****8000" {
		t.Errorf("u1 nickname = %q, want the phone", got["u1"])
	}
	if got["u2"] != "" {
		t.Errorf("u2 nickname = %q, want empty (an IAM id is never a name)", got["u2"])
	}
	if got["u3"] != "张三" {
		t.Errorf("u3 nickname = %q", got["u3"])
	}
}
