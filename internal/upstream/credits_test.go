package upstream

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// 积分端点解析：statistics 桶、page 批次（两轮查询）、ops 活动（用完的不再查）、
// opengw 每日 token 池；快照派生（重置=套餐批次最早到期，退回 end_date）。
func TestFetchCreditSnapshot(t *testing.T) {
	snap := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case EpCreditStatistics:
			writeJSONEnvelope(w, `{"code":0,"data":{"package":{"package_name_cn":"旗舰版"},"end_date":"2026-11-24 00:00:00","metrics":[
				{"name":"usageTotalPackageCredit","package_credit_amount":4500,"package_credit_remain":4300,"package_credit_expiring_amount":1000},
				{"name":"usageBasicPackageCredit","package_credit_amount":500,"package_credit_remain":400,"package_credit_expiring_amount":0},
				{"name":"usageOnDemandPackageCredit","package_credit_amount":0,"package_credit_remain":0},
				{"name":"usageBonusPackageCredit","package_credit_amount":4000,"package_credit_remain":3900,"package_credit_expiring_amount":1000}]}}`)
		case EpCreditPackagePage:
			var body struct {
				PackageTypes []string `json:"packageTypes"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if len(body.PackageTypes) > 0 && body.PackageTypes[0] == "basic" {
				_, _ = io.WriteString(w, `{"list":[{"packageType":"basic","creditAmount":500,"creditUsed":100,"expiredTime":"2026-11-24T00:00:00+00:00"}],"total":1}`)
				return
			}
			_, _ = io.WriteString(w, `{"list":[],"total":0}`)
		case EpOpsCreditOverview:
			_, _ = io.WriteString(w, `{"code":0,"data":{"items":[
				{"campaignId":4,"totalClaimed":4000,"totalConsumed":100,"totalExpired":0},
				{"campaignId":9,"totalClaimed":1000,"totalConsumed":1000,"totalExpired":0}]}}`)
		case EpOpsCreditCampaign + "/4":
			_, _ = io.WriteString(w, `{"code":0,"data":{"campaignId":4,"benefits":[
				{"status":"CONFIRMED","claimedAmount":4000,"consumedAmount":100,"expiredAmount":0,"expireAt":"2026-11-23 00:00:00"}]}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer snap.Close()
	ogw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != EpUserTokensBalance {
			http.NotFound(w, r)
			return
		}
		_, _ = io.WriteString(w, `{"error_code":"0000","result":{"daily_token_limit":1000000,"daily_tokens_used":200000}}`)
	}))
	defer ogw.Close()

	c := New(5 * time.Second)
	c.snapBase = snap.URL
	c.benefitBase = ogw.URL
	cred := SignCredential{AccessKeyID: "ak", SecretAccessKey: "sk", SecurityToken: "st"}

	stats, err := c.FetchCreditStatistics(context.Background(), cred)
	if err != nil {
		t.Fatalf("statistics: %v", err)
	}
	if stats.Package != "旗舰版" || len(stats.Buckets) != 3 {
		t.Fatalf("stats = %+v", stats)
	}

	batches, err := c.FetchCreditBatches(context.Background(), cred)
	if err != nil {
		t.Fatalf("batches: %v", err)
	}
	if len(batches) != 2 {
		t.Fatalf("batches = %+v, 期望 basic + 活动 bonus（用完的 campaign 9 不再查）", batches)
	}

	tokens, err := c.FetchTokenBalance(context.Background(), cred)
	if err != nil || tokens == nil || tokens.DailyLimit != 1000000 || tokens.DailyUsed != 200000 {
		t.Fatalf("token pool = %+v err=%v", tokens, err)
	}

	snapshot := BuildCreditSnapshot(stats, batches, tokens, time.UnixMilli(0).Add(time.Minute))
	if snapshot.ResetsAt != ParseVendorTimeString("2026-11-24T00:00:00+00:00") {
		t.Fatalf("resets_at = %d, 期望套餐批次最早到期", snapshot.ResetsAt)
	}
	if len(snapshot.Expiring) != 2 || snapshot.Expiring[0].At != ParseVendorTimeString("2026-11-23 00:00:00") {
		t.Fatalf("expiring = %+v", snapshot.Expiring)
	}
}

func writeJSONEnvelope(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, body)
}
