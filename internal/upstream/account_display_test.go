package upstream

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// resetPhoneCacheForTest 清空 6h 手机号缓存，避免用例互相影响。
func resetPhoneCacheForTest() {
	phoneMu.Lock()
	phoneCache = map[string]phoneEntry{}
	phoneMu.Unlock()
}

// maskPhone 与插件同口径：去掉 0086/+86 前缀，11 位以上才脱敏。
func TestMaskPhone(t *testing.T) {
	cases := map[string]string{
		"0086-13800138000":  "138****8000",
		"+86 138 0013 8000": "138****8000",
		"13800138000":       "138****8000",
		"":                  "",
		"12345":             "",
	}
	for raw, want := range cases {
		if got := MaskPhone(raw); got != want {
			t.Errorf("MaskPhone(%q) = %q, want %q", raw, got, want)
		}
	}
}

// IAM 写在名字位置上的 ID 形状一律识别；正常人名不受影响。
func TestLooksLikeIdentifier(t *testing.T) {
	for _, id := range []string{
		"iam::d1:user:hw123456789012345",
		"arn:aws:iam::123:user/x",
		"urn:ietf:params:oauth:jwt",
		"0123456789abcdef0123456789abcdef",
		"123e4567-e89b-12d3-a456-426614174000",
		"hw123456789012345",
	} {
		if !LooksLikeIdentifier(id) {
			t.Errorf("%q should look like an identifier", id)
		}
	}
	for _, name := range []string{"张三", "acct-a", "GT-Jailbreak_2016", "", "138****8000"} {
		if LooksLikeIdentifier(name) {
			t.Errorf("%q should not look like an identifier", name)
		}
	}
}

// displayNameOf 取第一个非空且不是 ID 的候选；全不合格时为空。
func TestDisplayNameOf(t *testing.T) {
	if got := DisplayNameOf("", "hw123456789012345", "张三"); got != "张三" {
		t.Errorf("DisplayNameOf = %q, want 张三", got)
	}
	if got := DisplayNameOf("138****8000", "张三"); got != "138****8000" {
		t.Errorf("phone should win: %q", got)
	}
	if got := DisplayNameOf("iam::d:user:x", "urn:y", "  "); got != "" {
		t.Errorf("all identifiers must leave an empty name: %q", got)
	}
}

// fetchAccountPhone 用账号自己的 STS 凭据签名 GET，成功结果缓存 6h。
func TestFetchAccountPhoneSignedAndCached(t *testing.T) {
	resetPhoneCacheForTest()
	oldBase, oldClient := iamUserBase, iamHTTPClient
	t.Cleanup(func() { iamUserBase, iamHTTPClient = oldBase, oldClient; resetPhoneCacheForTest() })

	hits := 0
	var gotPath, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		gotPath, gotAuth = r.URL.Path, r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"user":{"name":"hw12323","phone":"0086-13800138000"}}`))
	}))
	defer srv.Close()
	iamUserBase, iamHTTPClient = srv.URL+EpIAMUser, srv.Client()

	cred := SignCredential{AccessKeyID: "AK", SecretAccessKey: "SK", SecurityToken: "ST"}
	if got := FetchAccountPhone(cred, "pid-1"); got != "138****8000" {
		t.Fatalf("phone = %q", got)
	}
	if gotPath != EpIAMUser+"pid-1" {
		t.Errorf("path = %q", gotPath)
	}
	if !strings.HasPrefix(gotAuth, "SDK-HMAC-SHA256 ") {
		t.Errorf("request must be signed, got %q", gotAuth)
	}
	if got := FetchAccountPhone(cred, "pid-1"); got != "138****8000" || hits != 1 {
		t.Errorf("second read should hit the cache: phone=%q hits=%d", got, hits)
	}
}

// 无 principal_id、上游拒绝、应答无号码都返回空串，且失败也缓存。
func TestFetchAccountPhoneBestEffort(t *testing.T) {
	resetPhoneCacheForTest()
	oldBase, oldClient := iamUserBase, iamHTTPClient
	t.Cleanup(func() { iamUserBase, iamHTTPClient = oldBase, oldClient; resetPhoneCacheForTest() })

	cred := SignCredential{AccessKeyID: "AK", SecretAccessKey: "SK", SecurityToken: "ST"}
	called := false
	iamHTTPClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		called = true
		return &http.Response{StatusCode: http.StatusForbidden, Body: http.NoBody}, nil
	})}
	if got := FetchAccountPhone(cred, ""); got != "" || called {
		t.Errorf("no principal id must not call IAM: %q called=%v", got, called)
	}
	if got := FetchAccountPhone(cred, "pid-2"); got != "" {
		t.Errorf("refusal must give an empty phone: %q", got)
	}
	if got := FetchAccountPhone(cred, "pid-2"); got != "" || !called {
		t.Errorf("failure should be cached too: %q", got)
	}
}

// 身份解析按 0.2.6 过滤 ID 形状，并带出读手机号用的 principal_id。
func TestIdentityFromRefreshTokenFiltersIdentifiers(t *testing.T) {
	urn := "iam::d1:user:hw123456789012345"
	payload := `{"user_profile":{"principal_urn":"` + urn + `","principal_id":"pid-1","user_name":"` + urn + `","account_name":"hw123456789012345","domain_name":"` + urn + `"}}`
	tok := "h." + base64.RawURLEncoding.EncodeToString([]byte(payload)) + ".s"
	id := IdentityFromRefreshToken(tok)
	if id.ID != urn || id.Name != "" || id.IAMUserID != "pid-1" {
		t.Fatalf("identity = %+v", id)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
