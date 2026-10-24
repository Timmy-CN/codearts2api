package pool

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"codearts2api/internal/auth"
	"codearts2api/internal/upstream"
)

// refresh_token 是一次性的：另一个进程（cmd/apply 等）把磁盘上的凭证换新后，
// 本进程的刷新会撞「has been used」。此时应采纳磁盘上的最新凭证重试恢复，
// 而不是把账号禁用。
func TestRefreshTokenRecoversFromDisk(t *testing.T) {
	jwk, err := upstream.NewDPoPPrivateJWK()
	if err != nil {
		t.Fatalf("生成 DPoP 密钥: %v", err)
	}
	a := auth.New("u1", "user", "domain", "sts-old", "ak-old", "sk-old",
		time.Now().Add(time.Hour).Format(time.RFC3339), "rt-old", "verifier")
	a.SetDPoPPrivateKey(jwk)
	path := filepath.Join(t.TempDir(), "codearts-u1.json")
	if err := a.SetPathAndSave(path); err != nil {
		t.Fatalf("落盘凭证: %v", err)
	}
	// 模拟 cmd/apply / 另一个实例已把磁盘凭证换新（本进程内存里仍是 rt-old）。
	disk, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读凭证文件: %v", err)
	}
	var diskAuth map[string]any
	if err := json.Unmarshal(disk, &diskAuth); err != nil {
		t.Fatalf("解析凭证文件: %v", err)
	}
	diskAuth["refresh_token"] = "rt-new"
	diskAuth["access_key_id"] = "ak-new"
	diskAuth["secret_access_key"] = "sk-new"
	diskAuth["cloud_dragon_token"] = "sts-new"
	updated, _ := json.Marshal(diskAuth)
	if err := os.WriteFile(path, updated, 0o600); err != nil {
		t.Fatalf("模拟外部换新: %v", err)
	}

	var stsCalls atomic.Int32
	sts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("parse form: %v", err)
		}
		n := stsCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		switch n {
		case 1: // 旧 refresh_token 已被别处使用
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"invalid_grant","error_msg":"refresh token has been used"}`))
		case 2: // 磁盘上的新 refresh_token 刷新成功
			if got := r.Form.Get("refresh_token"); got != "rt-new" {
				t.Errorf("第二次刷新应使用磁盘上的 rt-new, got=%q", got)
			}
			_, _ = w.Write([]byte(`{"credentials":{"access_key_id":"ak-new","secret_access_key":"sk-new",` +
				`"security_token":"sts-new","expiration":"2026-12-01T00:00:00Z"},"refresh_token":"rt-new"}`))
		default:
			t.Errorf("STS 被调用 %d 次", n)
		}
	}))
	defer sts.Close()

	p, err := New([]*auth.Auth{a}, poolConfigWithSTS(sts.URL), "")
	if err != nil {
		t.Fatal(err)
	}
	if err := p.RefreshToken("u1"); err != nil {
		t.Fatalf("应从磁盘凭证恢复刷新: %v", err)
	}
	if calls := stsCalls.Load(); calls != 2 {
		t.Fatalf("STS 调用次数 = %d, 期望 2", calls)
	}
	if !p.Healthy("u1") {
		t.Fatal("恢复后账号应保持健康")
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), `"ak-new"`) || !strings.Contains(string(data), `"rt-new"`) {
		t.Fatalf("新凭证未回写磁盘: %s", data)
	}
}

func poolConfigWithSTS(stsHost string) Config {
	return Config{
		LoginConfig: upstream.LoginConfig{
			STSHost:       stsHost,
			ClientID:      "codearts-agent",
			PluginName:    "snap_AIIDE",
			PluginVersion: "5.2.0",
		},
		ErrThreshold:  3,
		SoftCooldown:  time.Minute,
		RefreshSkew:   30 * time.Minute,
		MaxConcurrent: 1,
	}
}
