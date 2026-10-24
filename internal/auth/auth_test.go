package auth

import "testing"

// user_id 可能是带冒号的 principal_urn：文件名必须净化（Windows 不允许冒号），
// 且映射稳定；空 id 退化为 unknown；普通 id 原样保留。
func TestFileNameSanitizesUnsafeChars(t *testing.T) {
	cases := []struct {
		name   string
		userID string
		want   string
	}{
		{"principal_urn", "iam::072974baba6b40a1a032f9cf27395a4b:user:GT-Jailbreak_2016",
			"codearts-iam__072974baba6b40a1a032f9cf27395a4b_user_GT-Jailbreak_2016.json"},
		{"空 id", "", "codearts-unknown.json"},
		{"普通数字 id", "7615755", "codearts-7615755.json"},
		{"全部非法字符", `a/b:c*d?e"f<g>h|i\j`, "codearts-a_b_c_d_e_f_g_h_i_j.json"},
	}
	for _, tc := range cases {
		a := New(tc.userID, "name", "domain", "tok", "ak", "sk", "2026-12-01T00:00:00Z", "", "")
		if got := a.FileName(); got != tc.want {
			t.Fatalf("%s: FileName = %q, 期望 %q", tc.name, got, tc.want)
		}
	}
}
