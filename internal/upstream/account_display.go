// account_display.go 账号显示名口径（插件 codearts 0.2.5 / 0.2.6）。
//
// 0.2.5：显示名优先用 IAM 里的脱敏手机号（138****8000），IAM 只读一次
// （6h 缓存，失败也缓存），accountId 仍是 URN，续期不重命名。
// 0.2.6：IAM 内部 ID 形状（iam::…、urn:、hw…、uuid、裸 hex）永远不当显示名；
// 候选全被跳过时名字为空，由调用方回退。
package upstream

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
)

// identifierPatterns 是 IAM 写在「名字」位置上的 ID 形状（插件 0.2.6 同表）。
var identifierPatterns = []*regexp.Regexp{
	regexp.MustCompile(`::`),                            // iam::<domain>:user:<name> / arn:…
	regexp.MustCompile(`(?i)^urn:`),                     // urn:…
	regexp.MustCompile(`(?i)^hw[0-9a-f]{6,}$`),          // 华为云 IAM 账号名，如 hw123456789012345
	regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-`), // uuid
	regexp.MustCompile(`(?i)^[0-9a-f]{16,}$`),           // 裸 hash，含 32 位 domain id
}

// LooksLikeIdentifier 判断该值是否只是内部 ID 而非人名（插件 looksLikeIdentifier）。
func LooksLikeIdentifier(value string) bool {
	s := strings.TrimSpace(value)
	if s == "" {
		return false
	}
	for _, re := range identifierPatterns {
		if re.MatchString(s) {
			return true
		}
	}
	return false
}

// DisplayNameOf 返回第一个「人能分辨」的候选：非空且不是 ID 形状。
// 全部不合格时返回空串，由调用方回退到供应商名/未命名（插件 displayNameOf）。
func DisplayNameOf(candidates ...string) string {
	for _, candidate := range candidates {
		name := strings.TrimSpace(candidate)
		if name != "" && !LooksLikeIdentifier(name) {
			return name
		}
	}
	return ""
}

// MaskPhone 把 IAM 的手机号写成卡片口径：0086-13800138000 / +86 138 0013 8000
// / 13800138000 → 138****8000；不足 11 位返回空串（插件 maskPhone）。
func MaskPhone(raw string) string {
	digits := make([]byte, 0, len(raw))
	for i := range raw {
		if raw[i] >= '0' && raw[i] <= '9' {
			digits = append(digits, raw[i])
		}
	}
	s := string(digits)
	if len(s) > 11 && strings.HasPrefix(s, "0086") {
		s = s[4:]
	} else if len(s) > 11 && strings.HasPrefix(s, "86") {
		s = s[2:]
	}
	if len(s) < 11 {
		return ""
	}
	return s[:3] + "****" + s[len(s)-4:]
}

// 手机号读取：每个 IAM 用户 6h 读一次，失败（空结果）也缓存，避免卡片刷新
// 每次都等满超时；条目超过上限时先清过期项。
const (
	iamPhoneTTL     = 6 * time.Hour
	iamPhoneCap     = 1024
	iamPhoneTimeout = 8 * time.Second
)

type phoneEntry struct {
	at    time.Time
	phone string
}

var (
	phoneMu    sync.Mutex
	phoneCache = map[string]phoneEntry{}

	// 测试可替换（同包测试直接改这两个变量）。
	iamUserBase   = IAMRegionHost + EpIAMUser
	iamHTTPClient = &http.Client{}
)

func rememberPhone(iamUserID, phone string) {
	now := time.Now()
	if len(phoneCache) >= iamPhoneCap {
		for k, v := range phoneCache {
			if now.Sub(v.at) >= iamPhoneTTL {
				delete(phoneCache, k)
			}
		}
		if len(phoneCache) >= iamPhoneCap*2 {
			phoneCache = map[string]phoneEntry{}
		}
	}
	phoneCache[iamUserID] = phoneEntry{at: now, phone: phone}
}

// FetchAccountPhone 用账号自己的 STS 凭据读 IAM 手机号并脱敏；尽力而为：
// 没有 principal_id、请求失败或应答没有可用号码时返回空串，绝不让登录失败。
// 结果（含失败）按 IAM 用户缓存 6h（插件 fetchAccountPhone）。
func FetchAccountPhone(cred SignCredential, iamUserID string) string {
	iamUserID = strings.TrimSpace(iamUserID)
	if iamUserID == "" || cred.AccessKeyID == "" || cred.SecurityToken == "" {
		return ""
	}
	phoneMu.Lock()
	if hit, ok := phoneCache[iamUserID]; ok && time.Since(hit.at) < iamPhoneTTL {
		phone := hit.phone
		phoneMu.Unlock()
		return phone
	}
	phoneMu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), iamPhoneTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, iamUserBase+url.PathEscape(iamUserID), nil)
	if err != nil {
		return ""
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Language", "zh-cn")
	signRequest(req, []byte{}, cred)
	resp, err := iamHTTPClient.Do(req)
	if err != nil {
		rememberPhone(iamUserID, "")
		return ""
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		rememberPhone(iamUserID, "")
		return ""
	}
	var out struct {
		User struct {
			Phone string `json:"phone"`
		} `json:"user"`
	}
	phone := ""
	if json.Unmarshal(raw, &out) == nil {
		phone = MaskPhone(out.User.Phone)
	}
	rememberPhone(iamUserID, phone)
	return phone
}

// IAMUserIDFromRefreshToken 取 IAM 用户 id（user_profile 的 principal_id，
// 退到 user_id），读手机号用；解不出返回空串。
func IAMUserIDFromRefreshToken(token string) string {
	_, _, principalID := parseUserProfileFromToken(token)
	return principalID
}
