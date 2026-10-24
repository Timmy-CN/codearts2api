// CodeArts Agent 云端客户端。
//
// 纯 HTTP：
//   - 登录：华为云 CodeArts OAuth2（PKCE + 本地回调）→ snap-manager /v1/oauth2/tokens
//     换 STS 临时 AK/SK + security_token；ticket 轮询为兜底通道
//   - 聊天：POST snap-access/v1/chat/chat，header x-auth-token=security_token，SSE 流式
//   - 刷新：POST /v1/oauth2/tokens grant_type=refresh_token
package upstream

import (
	"bufio"
	"bytes"
	"context"
	crand "crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ApiError 带业务 code 的上游错误。
type ApiError struct {
	Code    int
	Status  int
	Message string
	Path    string
}

func (e *ApiError) Error() string {
	return fmt.Sprintf("codearts api code=%d http=%d path=%s msg=%s", e.Code, e.Status, e.Path, e.Message)
}

// Credentials 登录返回的 STS 临时凭证。
type Credentials struct {
	AccessKeyID     string `json:"access_key_id"`
	SecretAccessKey string `json:"secret_access_key"`
	SecurityToken   string `json:"security_token"`
	Expiration      string `json:"expiration"`
}

// LegacyCredential ticket 通道（旧登录）返回的凭证字段。
type LegacyCredential struct {
	Access        string `json:"access"`
	Secret        string `json:"secret"`
	SecurityToken string `json:"securitytoken"`
	ExpiresAt     string `json:"expires_at"`
}

// TokenResponse oauth2/tokens 响应（含用户信息与凭证）。
type TokenResponse struct {
	UserID       string           `json:"user_id"`
	UserName     string           `json:"user_name"`
	DomainID     string           `json:"domain_id"`
	RefreshToken string           `json:"refresh_token"`
	Credentials  Credentials      `json:"credentials"`
	Credential   LegacyCredential `json:"credential"`
	// 错误字段（refresh 失败时上游会返回 error_code/error_msg）。
	Error        string `json:"error"`
	ErrorCode    string `json:"error_code"`
	ErrorMessage string `json:"error_msg"`
}

// RefreshTokenExpiredError 表示续期凭据已终态失效，需要重新登录；
// 它与网络/5xx 等可重试错误分开，避免无限刷新。
type RefreshTokenExpiredError struct {
	Status  int
	Message string
}

func (e *RefreshTokenExpiredError) Error() string { return e.Message }

// LoginConfig 登录相关配置。
type LoginConfig struct {
	ClientID      string
	PortalHost    string
	SnapManager   string
	STSHost       string
	RedirectPath  string // 本地回调路径
	PluginName    string
	PluginVersion string
}

// DefaultLoginConfig 生产默认值（逆向自 huaweicloud.authentication 扩展）。
func DefaultLoginConfig() LoginConfig {
	return LoginConfig{
		ClientID:      CLIENT_ID,
		PortalHost:    PortalHost,
		SnapManager:   SnapManagerHost,
		STSHost:       STSHost,
		RedirectPath:  "/oauth/callback",
		PluginName:    "snap_AIIDE",
		PluginVersion: "5.2.0",
	}
}

// Client CodeArts 云 API 客户端。
type Client struct {
	http       *http.Client // 短请求，带总超时
	streamHTTP *http.Client // SSE 长流，无总超时

	// 上游主机，生产固定为华为云；测试可替换为 httptest 服务。
	snapBase    string
	benefitBase string
	// chatURL chat-completions 完整地址覆盖（测试注入用，留空走 snapBase）。
	chatURL string

	// claimAuto 模型发现时是否自动领取限时福利。
	// 默认开启：不领取时福利模型一律返回 InferHub.4004.200 benefit not found
	// （实测 2026-09-15），服务商把「领取」做成幂等操作，官方客户端打开模型
	// 菜单也会调用；关闭后福利模型列表仍在，但调用必然失败。
	claimAuto atomic.Bool

	// 分来源失败退避：同一账号的 builtin/福利接口打不通时短期内不再重试，
	// 避免每次 /v1/models 刷新都空等超时。
	srcMu   sync.Mutex
	agentMu sync.Mutex
	agentID string // 上次成功应答模型列表的 agent id（state.agentId 同口径）
	srcFail map[string]time.Time
}

// srcFailCooldown 上游某一来源连续失败后的退避窗口。
const srcFailCooldown = 5 * time.Minute

// New 构造客户端。
func New(timeout time.Duration) *Client {
	if timeout <= 0 {
		timeout = 120 * time.Second
	}
	tr := noRedirectTransport{base: &http.Transport{
		MaxIdleConns:          20,
		MaxIdleConnsPerHost:   4,
		IdleConnTimeout:       90 * time.Second,
		ResponseHeaderTimeout: 120 * time.Second,
	}}
	c := &Client{
		http:        &http.Client{Timeout: timeout, Transport: tr},
		streamHTTP:  &http.Client{Transport: tr},
		snapBase:    SnapEngineApiHost,
		benefitBase: BenefitHost,
		srcFail:     map[string]time.Time{},
	}
	c.claimAuto.Store(true)
	return c
}

// noRedirectTransport 阻止签名/token 请求跟随重定向，并把 3xx 统一转成 502
// 错误响应：AK 签名头与 security token 绝不随跳转外泄（插件 redirect:"error"
// 同口径）。包装在共享 Transport 上，覆盖经过该客户端的全部请求。
type noRedirectTransport struct{ base http.RoundTripper }

func (t noRedirectTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(req)
	if err != nil || resp == nil {
		return resp, err
	}
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		loc := truncateStr(resp.Header.Get("Location"), 200)
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<10))
		resp.Body.Close()
		return &http.Response{
			StatusCode: http.StatusBadGateway,
			Status:     "502 Bad Gateway",
			Proto:      resp.Proto,
			Header:     http.Header{},
			Body: io.NopCloser(strings.NewReader(fmt.Sprintf(
				`{"error":"unexpected redirect %d: %s"}`, resp.StatusCode, loc))),
			Request: resp.Request,
		}, nil
	}
	return resp, nil
}

// NewWithChatEndpoint 构造指向自定义 chat-completions 端点的客户端。
// 主要用于端到端测试：仅替换外部 CodeArts HTTP 边界，其余请求链保持真实。
func NewWithChatEndpoint(timeout time.Duration, endpoint string) *Client {
	c := New(timeout)
	if strings.TrimSpace(endpoint) != "" {
		c.chatURL = endpoint
	}
	return c
}

// chatEndpoint 返回本次请求要用的 chat-completions 地址。
func (c *Client) chatEndpoint() string {
	if c.chatURL != "" {
		return c.chatURL
	}
	return c.snapURL(EpChatV2)
}

// SetBenefitAutoClaim 设置模型发现时是否自动领取限时福利（默认 false）。
// 关闭时 /v1/models 只读不写，需要领取请显式调用 ClaimBenefit（cmd/models -claim）。
func (c *Client) SetBenefitAutoClaim(v bool) { c.claimAuto.Store(v) }

// BenefitAutoClaim 报告是否开启自动领取。
func (c *Client) BenefitAutoClaim() bool { return c.claimAuto.Load() }

// snapURL 拼接 snap-access 主机地址。
func (c *Client) snapURL(path string) string { return c.snapBase + path }

// benefitURL 拼接福利网关主机地址。
func (c *Client) benefitURL(path string) string { return c.benefitBase + path }

// srcBlocked 报告某来源是否处于失败退避窗口内。
func (c *Client) srcBlocked(key string) bool {
	c.srcMu.Lock()
	defer c.srcMu.Unlock()
	until, ok := c.srcFail[key]
	if !ok {
		return false
	}
	if time.Now().After(until) {
		delete(c.srcFail, key)
		return false
	}
	return true
}

// noteSrcFail 记录某来源失败，进入退避窗口。
func (c *Client) noteSrcFail(key string) {
	c.srcMu.Lock()
	defer c.srcMu.Unlock()
	if c.srcFail == nil {
		c.srcFail = map[string]time.Time{}
	}
	c.srcFail[key] = time.Now().Add(srcFailCooldown)
}

// clearSrcFail 清空某来源的失败退避。
func (c *Client) clearSrcFail(key string) {
	c.srcMu.Lock()
	defer c.srcMu.Unlock()
	delete(c.srcFail, key)
}

// ---------------------------------------------------------------------------
// 认证
// ---------------------------------------------------------------------------

// BuildAuthorizeURL 构造 portal 登录链接（PKCE）。
// ticketID 客户端生成的随机 hex；port 为本地回调端口。
func (c *Client) BuildAuthorizeURL(cfg LoginConfig, ticketID, codeChallenge, codeChallengeMethod string, port int) string {
	_ = codeChallengeMethod // Portal 只识别实际插件使用的 "SHA-256"。
	q := url.Values{}
	q.Set("theme", "2")
	q.Set("locale", "zh-cn")
	q.Set("uri_scheme", cfg.ClientID)
	q.Set("client_id", cfg.ClientID)
	q.Set("port", fmt.Sprint(port))
	q.Set("code_challenge", codeChallenge)
	q.Set("code_challenge_method", "SHA-256")
	q.Set("ticket_id", ticketID)
	q.Set("plugin-name", cfg.PluginName)
	q.Set("plugin-version", cfg.PluginVersion)
	return cfg.PortalHost + "/authorize?" + q.Encode()
}

// ExchangeCode 用授权码换 token（OAuth2 authorization_code）。
//
// dpopPrivateJWK 可传 nil：此时随机生成一对，并把公钥随请求发出。调用方必须把
// 同一对私钥与 refresh_token 一起持久化，否则后续刷新会被 STS 拒
// （refresh_token 与 DPoP 公钥绑定）。
func (c *Client) ExchangeCode(ctx context.Context, cfg LoginConfig, code, codeVerifier string, port int, dpopPrivateJWK DPoPPrivateJWK) (*TokenResponse, error) {
	form := url.Values{}
	form.Set("client_id", cfg.ClientID)
	form.Set("code", code)
	form.Set("code_verifier", codeVerifier)
	form.Set("grant_type", "authorization_code")
	form.Set("redirect_uri", fmt.Sprintf("http://127.0.0.1:%d%s", port, cfg.RedirectPath))
	kp, err := dpopKeyPairFor(dpopPrivateJWK)
	if err != nil {
		return nil, err
	}
	return c.requestToken(ctx, cfg, form, kp)
}

// RefreshToken 用 refresh_token 换新凭证。
//
// 必须传签发该 refresh_token 时使用的 DPoP 私钥；传 nil 会退化为新密钥，
// 在服务端校验 DPoP 绑定时会 400 invalid refresh token: InvalidDPoPHeader。
func (c *Client) RefreshToken(ctx context.Context, cfg LoginConfig, refreshToken, codeVerifier string, dpopPrivateJWK DPoPPrivateJWK) (*TokenResponse, error) {
	form := url.Values{}
	form.Set("client_id", cfg.ClientID)
	form.Set("code_verifier", codeVerifier)
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", refreshToken)
	kp, err := dpopKeyPairFor(dpopPrivateJWK)
	if err != nil {
		return nil, err
	}
	return c.requestToken(ctx, cfg, form, kp)
}

// dpopKeyPairFor 用给定私钥恢复密钥对；未提供（旧凭证）时随机生成。
func dpopKeyPairFor(jwk DPoPPrivateJWK) (*dpopKeyPair, error) {
	if len(jwk) > 0 && jwk["d"] != "" {
		kp, err := dpopKeyPairFromPrivateJWK(jwk)
		if err != nil {
			return nil, fmt.Errorf("restore dpop keypair: %w", err)
		}
		return kp, nil
	}
	kp, err := newDpopKeyPair()
	if err != nil {
		return nil, fmt.Errorf("dpop keypair: %w", err)
	}
	return kp, nil
}

// requestToken 向 STS 令牌端点发 form + DPoP 请求。
func (c *Client) requestToken(ctx context.Context, cfg LoginConfig, form url.Values, kp *dpopKeyPair) (*TokenResponse, error) {
	url := cfg.STSHost + EpOAuthTokens
	proof, err := signDpopProof(kp, url)
	if err != nil {
		return nil, fmt.Errorf("dpop proof: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("DPoP", proof)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode >= 400 {
		// 刷新通道的「终态失效」要单独识别：继续重试没有意义，必须重新登录。
		if form.Get("grant_type") == "refresh_token" {
			var tokenErr TokenResponse
			_ = json.Unmarshal(raw, &tokenErr)
			code := tokenErr.ErrorCode
			if tokenErr.Error == "invalid_grant" || strings.Contains(code, "ExpiredRefreshToken") ||
				strings.Contains(code, "InvalidDPoPHeader") || strings.Contains(code, "invalid client id") ||
				strings.Contains(strings.ToLower(tokenErr.ErrorMessage), "has been used") {
				return nil, &RefreshTokenExpiredError{
					Status: resp.StatusCode,
					Message: fmt.Sprintf("refresh_token expired or rejected: http=%d error=%s code=%s msg=%s",
						resp.StatusCode, tokenErr.Error, tokenErr.ErrorCode, truncateStr(tokenErr.ErrorMessage, 160)),
				}
			}
		}
		return nil, &ApiError{Code: resp.StatusCode, Status: resp.StatusCode, Message: truncateStr(string(raw), 300), Path: EpOAuthTokens}
	}
	var out TokenResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("parse token response: %w body=%s", err, truncateStr(string(raw), 200))
	}
	if out.Credentials.SecurityToken == "" {
		// 兼容 ticket/旧通道字段（legacy）
		if out.Credential.SecurityToken != "" {
			out.Credentials = Credentials{
				AccessKeyID:     out.Credential.Access,
				SecretAccessKey: out.Credential.Secret,
				SecurityToken:   out.Credential.SecurityToken,
				Expiration:      out.Credential.ExpiresAt,
			}
			return &out, nil
		}
		return nil, fmt.Errorf("token response missing credentials: %s", truncateStr(string(raw), 200))
	}
	return &out, nil
}

// PollTicket 轮询登录结果（兜底通道）。
func (c *Client) PollTicket(ctx context.Context, cfg LoginConfig, ticketID, secret string) (*TokenResponse, error) {
	path := EpLoginTicket + "?ticket_id=" + url.QueryEscape(ticketID) + "&secret=" + url.QueryEscape(secret)
	headers := map[string]string{
		"plugin-name":    cfg.PluginName,
		"plugin-version": cfg.PluginVersion,
	}
	var out TokenResponse
	if err := c.doJSON(ctx, http.MethodGet, cfg.SnapManager, path, headers, nil, &out); err != nil {
		return nil, err
	}
	// 旧通道凭证归一化
	if out.Credentials.SecurityToken == "" && out.Credential.SecurityToken != "" {
		out.Credentials = Credentials{
			AccessKeyID:     out.Credential.Access,
			SecretAccessKey: out.Credential.Secret,
			SecurityToken:   out.Credential.SecurityToken,
			Expiration:      out.Credential.ExpiresAt,
		}
	}
	return &out, nil
}

// ---------------------------------------------------------------------------
// 聊天
// ---------------------------------------------------------------------------

// ChatMessage 单条消息（实测：Anthropic 风格内容块，无 role）。
type ChatToolFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type ChatToolCall struct {
	Index    int              `json:"index,omitempty"`
	ID       string           `json:"id,omitempty"`
	Type     string           `json:"type,omitempty"`
	Function ChatToolFunction `json:"function"`
}

// ChatMessage 是 /api/v2/chat/completions 的原生 OpenAI 消息。
//
// Type/Text 只保留给 cmd/probe 等旧调试入口；服务请求使用
// Role/Content/ReasoningContent/ToolCalls/ToolCallID，不再把多轮对话压成单条 user prompt。
type ChatMessage struct {
	Role             string         `json:"role,omitempty"`
	Content          any            `json:"content"`
	ReasoningContent *string        `json:"reasoning_content,omitempty"`
	ToolCalls        []ChatToolCall `json:"tool_calls,omitempty"`
	ToolCallID       string         `json:"tool_call_id,omitempty"`
	Name             string         `json:"name,omitempty"`

	Type string `json:"-"` // legacy debug input
	Text string `json:"-"` // legacy debug input
}

// ChatOptions 是需要原样传给 OpenAI 兼容上游的可选生成参数。
type ChatOptions struct {
	ReasoningEffort string
	MaxTokens       *int
	Temperature     *float64
	TopP            *float64
	Tools           []map[string]any
	ToolChoice      any
	// Thinking 未指定（nil）时注入 thinking:{type:"enabled"}（官方客户端行为）；
	// 显式 false 不注入；true 强制注入。
	Thinking *bool
}

// CanonicalModel 把用户友好模型 ID 映射为上游 InferHub 注册的模型 ID。
//
// 上游按注册名精确匹配（区分大小写），而客户端习惯写小写。动态发现的模型
// （见 models.go 的 known 索引）优先做大小写不敏感归一；旧版小写别名走下面的
// 静态映射兜底；都不认识就原样透传。
func CanonicalModel(id string) string {
	if exact, ok := lookupKnownModel(id); ok {
		return exact
	}
	switch id {
	case "snap-chat", "glm-5.2":
		return "GLM-5.2"
	case "glm-5.1":
		return "GLM-5.1"
	case "glm-4.7":
		return "GLM-4.7"
	case "qwen3-vl-235b":
		return "Qwen3-VL-235B"
	case "qwen3.5-397b-a17b-vl":
		return "Qwen3.5-397B-A17B-VL"
	case "qwen3.6-27b-vl":
		return "Qwen3.6-27B-VL"
	default:
		return id
	}
}

// ChatHeadersV2 组装 /api/v2/chat/completions 请求头。
// 与官方 AgentKernel 一致：x-auth-token 鉴权 + AK/SK 签名，无 Agent-Type。
func ChatHeadersV2(token, traceID, language string) map[string]string {
	if traceID == "" {
		traceID = fmt.Sprintf("%x", time.Now().UnixNano())
	}
	if language == "" {
		language = "zh-cn"
	}
	return map[string]string{
		"Content-Type":    "application/json",
		"Accept":          "text/event-stream",
		"x-auth-token":    token,
		"x-snap-traceid":  traceID,
		"X-Language":      language,
		"app-id":          "CodeAgent3.0",
		"is_confidential": "false",
	}
}

// ChatStream 发送 /api/v2/chat/completions（OpenAI 兼容，AK/SK 签名 + x-auth-token）
// 并返回 SSE 流（调用方负责 Close）。
//
// benefit 由调用方按「发起请求的账号」判定（见 IsBenefitModel）：限时福利模型
// 必须带 maas_type: benefit 头，判定依据是账号自己的模型目录，不能全局共享。
func (c *Client) ChatStream(ctx context.Context, chatID string, messages []ChatMessage, traceID string, cred SignCredential, userName string, model string, benefit bool) (io.ReadCloser, error) {
	return c.ChatStreamWithOptions(ctx, chatID, messages, traceID, cred, userName, model, ChatOptions{}, benefit, "")
}

// ChatStreamWithOptions 在基础聊天请求上附加推理等级与采样参数。
//
// sessionKey 是账号的稳定标识（如 user_id）：据此派生 x-ot-session-id——网关按
// 该头统计单账号 3 个并发会话，同账号共用一个会话 id 才不会被槽位 ~1 分钟的
// 释放滞后顶满（插件实测：不带此头的请求各自占一个新会话）。
func (c *Client) ChatStreamWithOptions(ctx context.Context, chatID string, messages []ChatMessage, traceID string, cred SignCredential, userName string, model string, opts ChatOptions, benefit bool, sessionKey string) (io.ReadCloser, error) {
	body := chatBodyV2(chatID, messages, model, opts)
	otSession := ""
	if sessionKey != "" {
		otSession = stableSessionID(sessionKey)
	}
	return c.sendChatV2(ctx, body, traceID, cred, cred.SecurityToken, chatID, sessionIDFor(chatID), benefit, otSession)
}

// stableSessionID 由账号稳定标识派生 32 hex 的网关会话 id（跨进程、跨刷新一致）。
func stableSessionID(key string) string {
	sum := sha256.Sum256([]byte("codearts-account-session:" + key))
	return hex.EncodeToString(sum[:16])
}

// chatBodyV2 组装 /api/v2/chat/completions 请求体（原生 OpenAI 形状）。
func chatBodyV2(chatID string, messages []ChatMessage, model string, opts ChatOptions) map[string]any {
	body := map[string]any{
		"model":            CanonicalModel(model),
		"stream":           true,
		"messages":         chatMessagesToOpenAI(messages),
		"prompt_cache_key": chatID,
		"tool_stream":      true,
	}
	if chatID != "" {
		body["chat_id"] = chatID
	}
	if opts.ReasoningEffort != "" {
		body["reasoning_effort"] = opts.ReasoningEffort
	}
	if opts.MaxTokens != nil {
		mt := *opts.MaxTokens
		if mt > MaxOutputTokens {
			mt = MaxOutputTokens // 超上限会被上游拒绝，钳到最大值
		}
		body["max_tokens"] = mt
	}
	if opts.Thinking == nil || *opts.Thinking {
		body["thinking"] = map[string]any{"type": "enabled"}
	}
	if opts.Temperature != nil {
		body["temperature"] = *opts.Temperature
	}
	if opts.TopP != nil {
		body["top_p"] = *opts.TopP
	}
	if len(opts.Tools) > 0 {
		body["tools"] = opts.Tools
		if opts.ToolChoice != nil {
			body["tool_choice"] = opts.ToolChoice
		}
	}
	return body
}

// chatMessagesToOpenAI 保留标准 role/tool_calls/tool 结果；只对旧调试
// 入口的 Type/Text 消息降级为 user 文本。
func chatMessagesToOpenAI(msgs []ChatMessage) []map[string]any {
	out := make([]map[string]any, 0, len(msgs))
	for _, m := range msgs {
		if m.Role == "" {
			out = append(out, map[string]any{"role": "user", "content": m.Text})
			continue
		}
		wire := map[string]any{"role": m.Role, "content": m.Content}
		if m.ReasoningContent != nil {
			wire["reasoning_content"] = *m.ReasoningContent
		}
		if len(m.ToolCalls) > 0 {
			wire["tool_calls"] = m.ToolCalls
		}
		if m.ToolCallID != "" {
			wire["tool_call_id"] = m.ToolCallID
		}
		if m.Name != "" {
			wire["name"] = m.Name
		}
		out = append(out, wire)
	}
	return out
}

// sessionIDFor 由 chatID 派生的稳定会话标识（prompt cache 亲和）。
func sessionIDFor(chatID string) string {
	sum := sha256.Sum256([]byte("codearts-session:" + chatID))
	return hex.EncodeToString(sum[:16])
}

// SendChatV2 发送自定义 OpenAI 兼容 body 到 /api/v2/chat/completions。
// 鉴权：x-auth-token（STS security token）+ 华为云 SDK-HMAC-SHA256 AK/SK 签名。
// benefit=true 时追加 maas_type: benefit（限时福利模型路由），该头在签名前设置，
// 计入 SignedHeaders。
func (c *Client) SendChatV2(ctx context.Context, body map[string]any, traceID string, cred SignCredential, userToken string, benefit bool) (io.ReadCloser, error) {
	return c.sendChatV2(ctx, body, traceID, cred, userToken, "", "", benefit, "")
}

// sendChatV2 发送聊天请求并返回 SSE 流。
//
// chatID/sessionID 作为请求亲和与 prompt cache 标识（不代替 messages 里的语义历史）。
// benefit 由调用方按账号判定，maas_type 在签名前写入，计入 SignedHeaders。
func (c *Client) sendChatV2(ctx context.Context, body map[string]any, traceID string, cred SignCredential, userToken, chatID, sessionID string, benefit bool, otSession string) (io.ReadCloser, error) {
	raw, _ := json.Marshal(body)
	buildReq := func() (*http.Request, error) {
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.chatEndpoint(), bytes.NewReader(raw))
		if err != nil {
			return nil, err
		}
		for k, v := range ChatHeadersV2(userToken, traceID, "zh-cn") {
			httpReq.Header.Set(k, v)
		}
		if benefit {
			httpReq.Header.Set(HeaderMaasType, MaasBenefit)
		}
		signRequest(httpReq, raw, cred)
		if chatID != "" {
			httpReq.Header.Set("Chat-Id", chatID)
		}
		if sessionID != "" {
			httpReq.Header.Set("Session-Id", sessionID)
		}
		// 网关按 x-ot-session-id 统计并发会话：同账号全部聊天共用一个稳定会话
		// id，user-session-id 同值；trace/span 每次随机（官方客户端同形态，
		// 与 Chat-Id 一样在签名后追加；x-ot-parent-session-id 恒为空，Go 的
		// net/http 不发送空值头，等价于缺省不携带）。
		if otSession != "" {
			httpReq.Header.Set("x-ot-session-id", otSession)
			httpReq.Header.Set("user-session-id", otSession)
			httpReq.Header.Set("x-ot-trace-id", randomHexLower(16))
			httpReq.Header.Set("x-ot-span-id", randomHexLower(8))
		}
		return httpReq, nil
	}
	// 网关排队（插件同口径）：429 + queue-status:queued + trace-id → 轮询
	// trace-queues 直到 promoted 再重发；预算耗尽/被移出队列则 DELETE 退出。
	deadline := time.Now().Add(QueueBudget)
	for {
		httpReq, err := buildReq()
		if err != nil {
			return nil, err
		}
		resp, err := c.streamHTTP.Do(httpReq)
		if err != nil {
			return nil, err
		}
		if queued := queueInfoOf(resp); queued != nil {
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
			resp.Body.Close()
			if !ctxDeadlineOk(ctx, deadline) {
				return nil, &ApiError{Code: http.StatusTooManyRequests, Status: http.StatusTooManyRequests,
					Message: "排队等待超时或已被移出队列，请稍后再试", Path: EpChatV2}
			}
			if c.waitInQueue(ctx, cred, queued, deadline) == "promoted" {
				continue
			}
			return nil, &ApiError{Code: http.StatusTooManyRequests, Status: http.StatusTooManyRequests,
				Message: "排队等待超时或已被移出队列，请稍后再试", Path: EpChatV2}
		}
		if resp.StatusCode >= 400 {
			rawBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			resp.Body.Close()
			return nil, &ApiError{Code: resp.StatusCode, Status: resp.StatusCode, Message: truncateStr(string(rawBody), 200), Path: EpChatV2}
		}
		return preflightSSE(resp.Body)
	}
}

// ctxDeadlineOk 报告当前时刻是否仍在排队预算与请求截止之内。
func ctxDeadlineOk(ctx context.Context, deadline time.Time) bool {
	if time.Now().After(deadline) {
		return false
	}
	if d, ok := ctx.Deadline(); ok && time.Now().After(d) {
		return false
	}
	return ctx.Err() == nil
}

// queuedTrace 是一次「网关已排队」应答（429 + queue-status:queued + trace-id）。
type queuedTrace struct {
	TraceID    string
	RetryAfter time.Duration
}

func queueInfoOf(resp *http.Response) *queuedTrace {
	if !strings.EqualFold(strings.TrimSpace(resp.Header.Get("queue-status")), "queued") {
		return nil
	}
	trace := strings.TrimSpace(resp.Header.Get("trace-id"))
	if trace == "" {
		return nil
	}
	after := 5 * time.Second
	if raw := strings.TrimSpace(resp.Header.Get("retry-after")); raw != "" {
		if sec, err := strconv.ParseFloat(raw, 64); err == nil && sec >= 0 {
			after = time.Duration(sec * float64(time.Second))
		}
	}
	return &queuedTrace{TraceID: trace, RetryAfter: after}
}

// waitInQueue 按插件口径在网关队列里等待：每轮隔 retry-after 轮询
// trace-queues/{id}，promoted 返回可重发；not_found/error/连续 3 次轮询失败/
// 预算耗尽则 DELETE 退出（返回 gone）。
func (c *Client) waitInQueue(ctx context.Context, cred SignCredential, q *queuedTrace, deadline time.Time) string {
	leave := func() {
		leaveCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = c.traceQueueCall(leaveCtx, http.MethodDelete, q.TraceID, cred)
	}
	retryAfter := q.RetryAfter
	failures := 0
	for {
		if !ctxDeadlineOk(ctx, deadline) {
			leave()
			return "gone"
		}
		select {
		case <-time.After(retryAfter):
		case <-ctx.Done():
			leave()
			return "gone"
		}
		raw, err := c.traceQueueCall(ctx, http.MethodGet, q.TraceID, cred)
		if err != nil {
			failures++
			if failures >= 3 {
				leave()
				return "gone"
			}
			retryAfter = 2 * time.Second
			continue
		}
		var status struct {
			Status     string   `json:"status"`
			RetryAfter *float64 `json:"retry_after"`
		}
		_ = json.Unmarshal(raw, &status)
		switch status.Status {
		case "promoted":
			return "promoted"
		case "not_found", "error":
			leave()
			return "gone"
		}
		if status.RetryAfter != nil && *status.RetryAfter >= 0 {
			retryAfter = time.Duration(*status.RetryAfter * float64(time.Second))
		}
	}
}

// traceQueueCall 对排队 trace 做一次签名 GET/DELETE（空 body，无额外路由头）。
func (c *Client) traceQueueCall(ctx context.Context, method, traceID string, cred SignCredential) (json.RawMessage, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.snapURL(EpTraceQueues+"/"+traceID), nil)
	if err != nil {
		return nil, err
	}
	signRequest(req, []byte{}, cred)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return nil, &ApiError{Code: resp.StatusCode, Status: resp.StatusCode, Message: truncateStr(string(raw), 200), Path: EpTraceQueues}
	}
	return json.RawMessage(raw), nil
}

type replayReadCloser struct {
	io.Reader
	io.Closer
}

// preflightSSE 只读到首个 data 事件：正常事件原样回放，不破坏流式；
// HTTP 200 里内嵌的 CodeArts 排队/TPM 错误则在响应头发给客户端前
// 转为 ApiError，让上层可以重试整个请求。
func preflightSSE(body io.ReadCloser) (io.ReadCloser, error) {
	br := bufio.NewReaderSize(body, 1<<20)
	var prefix bytes.Buffer
	for prefix.Len() < 2<<20 {
		line, err := br.ReadString('\n')
		prefix.WriteString(line)
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "data:") {
			data := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
			if apiErr := embeddedSSEError(data); apiErr != nil {
				_ = body.Close()
				return nil, apiErr
			}
			return &replayReadCloser{Reader: io.MultiReader(bytes.NewReader(prefix.Bytes()), br), Closer: body}, nil
		}
		if err != nil {
			if err != io.EOF {
				_ = body.Close()
				return nil, err
			}
			return &replayReadCloser{Reader: bytes.NewReader(prefix.Bytes()), Closer: body}, nil
		}
	}
	return &replayReadCloser{Reader: io.MultiReader(bytes.NewReader(prefix.Bytes()), br), Closer: body}, nil
}

// embeddedSSEError 识别 HTTP 200 里内嵌的上游业务错误。
func embeddedSSEError(data string) *ApiError {
	if data == "" || data == "[DONE]" {
		return nil
	}
	var payload map[string]any
	if json.Unmarshal([]byte(data), &payload) != nil {
		return nil
	}
	code, _ := payload["error_code"].(string)
	if code == "" || code == "0" {
		return nil
	}
	message, _ := payload["error_msg"].(string)
	combined := strings.TrimSpace(code + " " + message)
	status := http.StatusBadGateway
	low := strings.ToLower(combined)
	switch {
	case strings.Contains(low, "429") || strings.Contains(low, "tm.00001041") ||
		strings.Contains(low, "tpm") || strings.Contains(low, "并发会话"):
		status = http.StatusTooManyRequests
	case strings.Contains(low, "002002009") || strings.Contains(low, "not registered") ||
		strings.Contains(low, "4004.200") || strings.Contains(low, "benefit not found"):
		// 模型级错误（该账号用不了这个模型）是客户端问题，不是上游故障。
		status = http.StatusBadRequest
	case strings.Contains(low, "inferhub.001001005") || strings.Contains(low, "request param is invalid"):
		// 请求参数无效（实测：tool_choice 对象形态触发）：客户端问题，不是账号故障。
		status = http.StatusBadRequest
	}
	return &ApiError{Code: status, Status: status, Message: truncateStr(combined, 300), Path: EpChatV2}
}

// ---------------------------------------------------------------------------
// 内部
// ---------------------------------------------------------------------------

func (c *Client) doJSON(ctx context.Context, method, baseURL, path string, headers map[string]string, body any, out any) error {
	var rd io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, baseURL+path, rd)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode >= 400 {
		return &ApiError{Code: resp.StatusCode, Status: resp.StatusCode, Message: truncateStr(string(raw), 300), Path: path}
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("parse %s: %w body=%s", path, err, truncateStr(string(raw), 200))
		}
	}
	return nil
}

// PKCE 生成 code_verifier / code_challenge（S256）。
func PKCE() (verifier, challenge string, err error) {
	b := make([]byte, 64)
	if _, err := crand.Read(b); err != nil {
		return "", "", err
	}
	verifier = base64.RawURLEncoding.EncodeToString(b)
	sum := sha256.Sum256([]byte(verifier))
	challenge = base64.RawURLEncoding.EncodeToString(sum[:])
	return verifier, challenge, nil
}

// RandomHex 生成 n 字节的 hex 随机串。
func RandomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := crand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// Log 上游日志。
func Log(format string, args ...any) {
	log.Printf("upstream: "+format, args...)
}

func truncateStr(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[:n]
	}
	return s
}

// CallerIdentity 用 STS 凭证查账号身份：GET {sts}/v5/caller-identity。
// 返回 user_id(principal_id)、user_name(principal_urn 尾段)、domain_id(account_id)。
func CallerIdentity(cred SignCredential) (uid, name, domain string, err error) {
	raw, err := signedGet(STSHost+EpCallerIdentity, cred, "")
	if err != nil {
		return "", "", "", err
	}
	var out struct {
		AccountID    string `json:"account_id"`
		PrincipalID  string `json:"principal_id"`
		PrincipalURN string `json:"principal_urn"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", "", "", fmt.Errorf("parse caller-identity: %w", err)
	}
	if i := strings.LastIndex(out.PrincipalURN, ":user:"); i >= 0 {
		name = out.PrincipalURN[i+len(":user:"):]
	}
	return out.PrincipalID, name, out.AccountID, nil
}

// CurrentUser 用 STS 凭证查账号身份：GET {snap}/snap-manager/v1/current/user。
func CurrentUser(cred SignCredential) (uid, name, domain string, err error) {
	raw, err := signedGet(SnapEngineApiHost+EpCurrentUser, cred, "")
	if err != nil {
		return "", "", "", err
	}
	var out struct {
		UserID   string `json:"user_id"`
		UserName string `json:"user_name"`
		DomainID string `json:"domain_id"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", "", "", fmt.Errorf("parse current/user: %w", err)
	}
	return out.UserID, out.UserName, out.DomainID, nil
}

// signedGet 发一个 AK/SK 签名 GET（可选 Agent-Type），返回响应体。
func signedGet(urlStr string, cred SignCredential, agentType string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, urlStr, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Language", "zh-cn")
	if agentType != "" {
		req.Header.Set("Agent-Type", agentType)
	}
	if cred.SecurityToken != "" {
		req.Header.Set("X-Security-Token", cred.SecurityToken)
	}
	signRequest(req, []byte{}, cred)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return nil, &ApiError{Code: resp.StatusCode, Status: resp.StatusCode, Message: truncateStr(string(raw), 300), Path: urlStr}
	}
	return raw, nil
}

// getSigned 发送带 Agent-Type 的 AK/SK 签名 GET（用于 agent-center 等管理接口）。
func (c *Client) getSigned(ctx context.Context, urlStr string, cred SignCredential, agentCenter bool) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, urlStr, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if agentCenter {
		req.Header.Set("Agent-Type", "AgentCenter")
	}
	req.Header.Set("X-Language", "zh-cn")
	if cred.SecurityToken != "" {
		req.Header.Set("X-Security-Token", cred.SecurityToken)
	}
	signRequest(req, []byte{}, cred)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, &ApiError{Code: resp.StatusCode, Status: resp.StatusCode, Message: truncateStr(string(raw), 300), Path: req.URL.Path}
	}
	return raw, nil
}

// AccountIdentity 是从 refresh_token JWT 里解出的账号身份。
type AccountIdentity struct {
	ID   string
	Name string
}

// IdentityFromRefreshToken 解析 refresh_token（JWT）user_profile claim 里的
// 账号身份，纯本地、不发网络请求。
//
// STS 的 code/refresh 响应不带用户字段，官方客户端就是从这里拿身份的。口径
// 与插件 accountIdentityOf 一致：id 依次取 principal_urn / account_name /
// account_id / user_id；name 依次取 user_name / account_name / domain_name；
// user_profile 可能是对象，也可能是再编码过一层的 base64 JSON 字符串。
// 解不出返回零值。
func IdentityFromRefreshToken(token string) AccountIdentity {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return AccountIdentity{}
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		if raw, err = base64.URLEncoding.DecodeString(parts[1]); err != nil {
			return AccountIdentity{}
		}
	}
	var payload struct {
		UserProfile json.RawMessage `json:"user_profile"`
	}
	if json.Unmarshal(raw, &payload) != nil || len(payload.UserProfile) == 0 {
		return AccountIdentity{}
	}
	if id, name := userProfileIdentity(payload.UserProfile); id != "" || name != "" {
		return AccountIdentity{ID: id, Name: name}
	}
	return AccountIdentity{}
}

// userProfileIdentity 从 user_profile 字段解出 (id, name)；字段本身可能是
// 对象，也可能是 JSON 字符串或再 base64 一层的 JSON 字符串。
func userProfileIdentity(raw json.RawMessage) (string, string) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", ""
	}
	if id, name := parseUserProfile(raw); id != "" || name != "" {
		return id, name
	}
	var text string
	if json.Unmarshal(raw, &text) != nil {
		return "", ""
	}
	if strings.HasPrefix(strings.TrimSpace(text), "{") {
		if id, name := parseUserProfile([]byte(text)); id != "" || name != "" {
			return id, name
		}
	}
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawURLEncoding, base64.RawStdEncoding} {
		if decoded, err := enc.DecodeString(strings.TrimSpace(text)); err == nil {
			if id, name := parseUserProfile(decoded); id != "" || name != "" {
				return id, name
			}
		}
	}
	return "", ""
}

func parseUserProfile(raw []byte) (string, string) {
	var profile struct {
		PrincipalURN string `json:"principal_urn"`
		AccountName  string `json:"account_name"`
		AccountID    string `json:"account_id"`
		UserID       string `json:"user_id"`
		UserName     string `json:"user_name"`
		DomainName   string `json:"domain_name"`
	}
	if json.Unmarshal(raw, &profile) != nil {
		return "", ""
	}
	id := firstNonEmpty(profile.PrincipalURN, profile.AccountName, profile.AccountID, profile.UserID)
	name := firstNonEmpty(profile.UserName, profile.AccountName, profile.DomainName)
	return id, name
}
