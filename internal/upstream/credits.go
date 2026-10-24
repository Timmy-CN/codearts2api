package upstream

// 积分/套餐面板数据。口径对照 magpie codearts 插件的 usage 实现：
//   - GET /snap-manager/v1/statistics/plugin：四个积分桶（总/基础/按需/赠送），
//     每桶 package_credit_amount / package_credit_remain / package_credit_expiring_amount；
//   - POST /snap-manager/v1/package/credit/page：套餐与自购批次（按
//     packageTypes 查两轮，响应是裸 {list,...} 无 code 信封）；
//   - GET /v1/ops/credit/overview + /{id}：签到/注册等活动积分批次
//     （left = totalClaimed - totalConsumed - totalExpired，已用完的活动不再查明细）；
//   - GET {opengw}/api/v1/user/tokens/balance：免费模型每日 token 池。
// 月度重置 = basic（套餐）批次最早到期；其余批次按到期时间给出作废提示。

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"time"
)

const (
	EpCreditStatistics  = "/snap-manager/v1/statistics/plugin"
	EpCreditPackagePage = "/snap-manager/v1/package/credit/page"
	EpOpsCreditOverview = "/v1/ops/credit/overview"
	EpOpsCreditCampaign = "/v1/ops/credit/campaign"
	EpUserTokensBalance = "/api/v1/user/tokens/balance"
)

// statistics.metrics[].name 的四个积分桶，与插件 CREDIT_METRIC_LABELS 一致。
const (
	MetricTotalCredit    = "usageTotalPackageCredit"
	MetricBasicCredit    = "usageBasicPackageCredit"
	MetricOnDemandCredit = "usageOnDemandPackageCredit"
	MetricBonusCredit    = "usageBonusPackageCredit"
)

var metricLabels = map[string]string{
	MetricTotalCredit:    "总积分",
	MetricBasicCredit:    "基础积分包",
	MetricOnDemandCredit: "按需积分包",
	MetricBonusCredit:    "赠送积分包",
}

// beijingTZ 是厂商「无时区时间串」的缺省时区（华为云在中国，按 +08:00 解）。
var beijingTZ = time.FixedZone("CST", 8*3600)

// vendorTime 解析厂商时间字段：数字 ≥1e12 按毫秒、≥1e9 按秒；字符串支持
// RFC3339 与「2006-01-02 15:04:05」（无时区按北京时间）。解不出返回 0。
func vendorTime(raw json.RawMessage) int64 {
	if len(raw) == 0 || string(raw) == "null" {
		return 0
	}
	var num json.Number
	if err := json.Unmarshal(raw, &num); err == nil {
		if v, err := num.Float64(); err == nil && v > 0 {
			if v >= 1e12 {
				return int64(v)
			}
			if v >= 1e9 {
				return int64(v * 1000)
			}
		}
		return 0
	}
	var text string
	if err := json.Unmarshal(raw, &text); err != nil || text == "" {
		return 0
	}
	return ParseVendorTimeString(text)
}

func ParseVendorTimeString(text string) int64 {
	text = trimSpace(text)
	if text == "" {
		return 0
	}
	if t, err := time.Parse(time.RFC3339, text); err == nil {
		return t.UnixMilli()
	}
	for _, layout := range []string{"2006-01-02T15:04:05", "2006-01-02 15:04:05"} {
		if t, err := time.ParseInLocation(layout, text, beijingTZ); err == nil {
			return t.UnixMilli()
		}
	}
	return 0
}

func trimSpace(s string) string {
	for len(s) > 0 && (s[0] == ' ' || s[0] == '\t' || s[0] == '\n' || s[0] == '\r') {
		s = s[1:]
	}
	for len(s) > 0 && (s[len(s)-1] == ' ' || s[len(s)-1] == '\t' || s[len(s)-1] == '\n' || s[len(s)-1] == '\r') {
		s = s[:len(s)-1]
	}
	return s
}

// CreditBucket 是 statistics 的一个积分桶。
type CreditBucket struct {
	Name     string  `json:"name"`
	Label    string  `json:"label"`
	Limit    float64 `json:"limit"`
	Remain   float64 `json:"remain"`
	Expiring float64 `json:"expiring"`
}

// CreditBatch 是一笔有独立到期时间的积分。
type CreditBatch struct {
	Metric    string  `json:"metric"`
	Amount    float64 `json:"amount"`
	Left      float64 `json:"left"`
	ExpiresAt int64   `json:"expires_at"` // 毫秒，0 = 未知
}

// ExpireHint 是一笔即将作废的积分。
type ExpireHint struct {
	At     int64   `json:"at"`     // 毫秒
	Credit float64 `json:"credit"` // 该笔剩余
}

// TokenPool 是免费模型的每日 token 池（maas_type: benefit 聊天消耗它）。
type TokenPool struct {
	DailyLimit float64 `json:"daily_token_limit"`
	DailyUsed  float64 `json:"daily_tokens_used"`
}

// CreditStatistics 是 statistics/plugin 的归一化结果。
type CreditStatistics struct {
	Package string         `json:"package,omitempty"` // 套餐名
	EndDate string         `json:"end_date,omitempty"`
	Buckets []CreditBucket `json:"buckets"`
}

// CreditSnapshot 是一次积分查询的完整快照（含派生的重置与作废提示）。
type CreditSnapshot struct {
	FetchedAt int64          `json:"fetched_at"`
	Package   string         `json:"package,omitempty"`
	EndDate   string         `json:"end_date,omitempty"`
	Buckets   []CreditBucket `json:"buckets"`
	Batches   []CreditBatch  `json:"batches,omitempty"`
	ResetsAt  int64          `json:"resets_at,omitempty"` // 月度重置时刻（毫秒）
	Expiring  []ExpireHint   `json:"expiring,omitempty"`  // 即将作废的批次（时间升序）
	TokenPool *TokenPool     `json:"token_pool,omitempty"`
	Error     string         `json:"error,omitempty"` // 部分数据读不到时的原因
}

// FetchCreditStatistics 查询积分桶（snap 信封响应，code!=0 报错）。
func (c *Client) FetchCreditStatistics(ctx context.Context, cred SignCredential) (*CreditStatistics, error) {
	raw, err := c.creditRequest(ctx, http.MethodGet, EpCreditStatistics, nil, cred)
	if err != nil {
		return nil, err
	}
	var data struct {
		Package struct {
			PackageNameCn string `json:"package_name_cn"`
			PackageNameEn string `json:"package_name_en"`
			SpecCode      string `json:"spec_code"`
		} `json:"package"`
		EndDate string `json:"end_date"`
		Metrics []struct {
			Name     string  `json:"name"`
			Amount   float64 `json:"package_credit_amount"`
			Remain   float64 `json:"package_credit_remain"`
			Expiring float64 `json:"package_credit_expiring_amount"`
		} `json:"metrics"`
	}
	if err := json.Unmarshal(raw, &data); err != nil {
		return nil, fmt.Errorf("parse credit statistics: %w", err)
	}
	stats := &CreditStatistics{
		Package: upFirstNonEmpty(data.Package.PackageNameCn, data.Package.PackageNameEn, data.Package.SpecCode),
		EndDate: data.EndDate,
		Buckets: []CreditBucket{},
	}
	for _, m := range data.Metrics {
		label, ok := metricLabels[m.Name]
		if !ok {
			continue
		}
		if m.Amount <= 0 && m.Remain <= 0 {
			continue // 空桶不展示
		}
		stats.Buckets = append(stats.Buckets, CreditBucket{
			Name: m.Name, Label: label, Limit: m.Amount, Remain: m.Remain, Expiring: m.Expiring,
		})
	}
	return stats, nil
}

// FetchCreditBatches 读取全部有独立到期时间的积分批次；完全读不到（两个来源
// 都失败）返回错误，部分来源失败返回已读到的批次。
func (c *Client) FetchCreditBatches(ctx context.Context, cred SignCredential) ([]CreditBatch, error) {
	batches := []CreditBatch{}
	reached := false
	queries := []map[string]any{
		{"pageNum": 1, "pageSize": 100, "sortField": "created_time", "sortOrder": "desc", "packageTypes": []string{"basic", "ondemand"}},
		{"pageNum": 1, "pageSize": 100, "packageTypes": []string{"bonus"}},
	}
	for _, query := range queries {
		raw, err := c.snapRawRequest(ctx, http.MethodPost, EpCreditPackagePage, query, cred)
		if err != nil {
			continue
		}
		var page struct {
			List []struct {
				PackageType  string          `json:"packageType"`
				CreditAmount float64         `json:"creditAmount"`
				CreditUsed   float64         `json:"creditUsed"`
				ExpiredTime  json.RawMessage `json:"expiredTime"`
			} `json:"list"`
		}
		if json.Unmarshal(raw, &page) != nil {
			continue
		}
		reached = true
		for _, p := range page.List {
			metric := metricOfPackageType(p.PackageType)
			if metric == "" {
				continue
			}
			batches = append(batches, CreditBatch{
				Metric:    metric,
				Amount:    p.CreditAmount,
				Left:      maxFloat(0, p.CreditAmount-p.CreditUsed),
				ExpiresAt: vendorTime(p.ExpiredTime),
			})
		}
	}
	// 活动积分（签到/注册）：总剩余>0 的活动才查明细，用完的不再问。
	if overview, err := c.FetchOpsCreditCampaignsBrief(ctx, cred); err == nil {
		reached = reached || overview != nil
		for _, brief := range overview {
			if brief.Left <= 0 || brief.CampaignID == "" {
				continue
			}
			grants, err := c.campaignGrants(ctx, cred, brief.CampaignID)
			if err != nil {
				continue
			}
			batches = append(batches, grants...)
		}
	}
	if !reached {
		return nil, fmt.Errorf("credit batches unreachable")
	}
	sort.SliceStable(batches, func(i, j int) bool { return batches[i].ExpiresAt < batches[j].ExpiresAt })
	return batches, nil
}

// campaignBrief 是 overview 里一条活动的剩余概况。
type campaignBrief struct {
	CampaignID string
	Left       float64
}

// FetchOpsCreditCampaignsBrief 拉活动总览，算出每个活动的剩余积分。
func (c *Client) FetchOpsCreditCampaignsBrief(ctx context.Context, cred SignCredential) ([]campaignBrief, error) {
	raw, err := c.creditRequest(ctx, http.MethodGet, EpOpsCreditOverview, nil, cred)
	if err != nil {
		return nil, err
	}
	var data struct {
		Items []struct {
			CampaignID    json.RawMessage `json:"campaignId"`
			TotalClaimed  float64         `json:"totalClaimed"`
			TotalConsumed float64         `json:"totalConsumed"`
			TotalExpired  float64         `json:"totalExpired"`
		} `json:"items"`
	}
	if err := json.Unmarshal(raw, &data); err != nil {
		return nil, err
	}
	out := make([]campaignBrief, 0, len(data.Items))
	for _, item := range data.Items {
		id := trimSpace(string(item.CampaignID))
		id = trimSpace(trimQuotes(id))
		if id == "" || id == "null" {
			continue
		}
		out = append(out, campaignBrief{
			CampaignID: id,
			Left:       item.TotalClaimed - item.TotalConsumed - item.TotalExpired,
		})
	}
	return out, nil
}

// campaignGrants 拉单个活动的积分批次（bonus 桶）。
func (c *Client) campaignGrants(ctx context.Context, cred SignCredential, id string) ([]CreditBatch, error) {
	raw, err := c.creditRequest(ctx, http.MethodGet,
		EpOpsCreditCampaign+"/"+id+"?pageNum=1&pageSize=100", nil, cred)
	if err != nil {
		return nil, err
	}
	var data struct {
		Benefits []struct {
			ClaimedAmount  float64         `json:"claimedAmount"`
			ConsumedAmount float64         `json:"consumedAmount"`
			ExpiredAmount  float64         `json:"expiredAmount"`
			ExpireAt       json.RawMessage `json:"expireAt"`
		} `json:"benefits"`
	}
	if err := json.Unmarshal(raw, &data); err != nil {
		return nil, err
	}
	batches := make([]CreditBatch, 0, len(data.Benefits))
	for _, b := range data.Benefits {
		batches = append(batches, CreditBatch{
			Metric:    MetricBonusCredit,
			Amount:    b.ClaimedAmount,
			Left:      maxFloat(0, b.ClaimedAmount-b.ConsumedAmount-b.ExpiredAmount),
			ExpiresAt: vendorTime(b.ExpireAt),
		})
	}
	return batches, nil
}

func metricOfPackageType(packageType string) string {
	switch packageType {
	case "basic":
		return MetricBasicCredit
	case "ondemand":
		return MetricOnDemandCredit
	case "bonus":
		return MetricBonusCredit
	}
	return ""
}

// snapRawRequest 发签名请求并返回原始响应体（不做 code 信封校验——
// package/credit/page 的响应没有信封）。
func (c *Client) snapRawRequest(ctx context.Context, method, path string, body any, cred SignCredential) (json.RawMessage, error) {
	var raw []byte
	var err error
	if body != nil {
		raw, err = json.Marshal(body)
		if err != nil {
			return nil, err
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, c.snapURL(path), bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Agent-Type", "PromptCenter")
	req.Header.Set("X-Language", "zh-cn")
	signRequest(req, raw, cred)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	response, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, &ApiError{Code: resp.StatusCode, Status: resp.StatusCode, Path: path, Message: truncateStr(string(response), 300)}
	}
	return json.RawMessage(response), nil
}

// FetchTokenBalance 查询免费模型的每日 token 池（opengw，error_code 信封）。
func (c *Client) FetchTokenBalance(ctx context.Context, cred SignCredential) (*TokenPool, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.benefitURL(EpUserTokensBalance), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
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
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return nil, &ApiError{Code: resp.StatusCode, Status: resp.StatusCode, Message: truncateStr(string(raw), 300), Path: EpUserTokensBalance}
	}
	var out struct {
		ErrorCode string `json:"error_code"`
		Result    struct {
			DailyTokenLimit float64 `json:"daily_token_limit"`
			DailyTokensUsed float64 `json:"daily_tokens_used"`
		} `json:"result"`
		Data struct {
			DailyTokenLimit float64 `json:"daily_token_limit"`
			DailyTokensUsed float64 `json:"daily_tokens_used"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("parse token balance: %w", err)
	}
	if out.ErrorCode != "0000" && out.ErrorCode != "" {
		return nil, fmt.Errorf("token balance failed: error_code=%s", out.ErrorCode)
	}
	limit := out.Result.DailyTokenLimit
	used := out.Result.DailyTokensUsed
	if limit == 0 && used == 0 {
		limit = out.Data.DailyTokenLimit
		used = out.Data.DailyTokensUsed
	}
	if limit <= 0 {
		return nil, nil // 账号没有每日池
	}
	return &TokenPool{DailyLimit: limit, DailyUsed: used}, nil
}

// BuildCreditSnapshot 组装快照并做派生：月度重置取 basic（套餐）批次最早到期
// （退回厂商 end_date）；即将作废列表取所有仍有剩余且未过期的批次，按到期时间
// 升序，最多 10 条。
func BuildCreditSnapshot(stats *CreditStatistics, batches []CreditBatch, tokens *TokenPool, now time.Time) *CreditSnapshot {
	snap := &CreditSnapshot{
		FetchedAt: now.UnixMilli(),
		Buckets:   []CreditBucket{},
		TokenPool: tokens,
	}
	if stats != nil {
		snap.Package = stats.Package
		snap.EndDate = stats.EndDate
		snap.Buckets = stats.Buckets
	}
	snap.Batches = batches
	resetsAt := int64(0)
	for _, b := range batches {
		if b.Metric == MetricBasicCredit && b.Left > 0 && b.ExpiresAt > now.UnixMilli() {
			if resetsAt == 0 || b.ExpiresAt < resetsAt {
				resetsAt = b.ExpiresAt
			}
		}
	}
	if resetsAt == 0 && stats != nil {
		resetsAt = ParseVendorTimeString(stats.EndDate)
	}
	snap.ResetsAt = resetsAt
	hints := make([]ExpireHint, 0, len(batches))
	for _, b := range batches {
		if b.Left > 0 && b.ExpiresAt > now.UnixMilli() {
			hints = append(hints, ExpireHint{At: b.ExpiresAt, Credit: b.Left})
		}
	}
	sort.SliceStable(hints, func(i, j int) bool { return hints[i].At < hints[j].At })
	if len(hints) > 10 {
		hints = hints[:10]
	}
	snap.Expiring = hints
	return snap
}

func maxFloat(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}

func trimQuotes(s string) string {
	if len(s) >= 2 && (s[0] == '"' && s[len(s)-1] == '"') {
		return s[1 : len(s)-1]
	}
	return s
}

func upFirstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
