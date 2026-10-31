package upstream

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

// 状态机 + 确认口径：只有本次 claim 失败的那条才补 confirm（插件 0.3.0）。
func TestCheckinTransitionsAndEligibility(t *testing.T) {
	tests := []struct {
		name, status string
		claimable    bool
		end          string
		start        string
		own          []int
		wantOK       bool
		wantWrites   []string
		wantCredits  float64
		wantMessage  string
	}{
		{name: "fresh", status: "ELIGIBLE", claimable: true, wantOK: true,
			wantWrites: []string{EpCreditClaim, EpCreditConfirm}, wantCredits: 1000},
		{name: "claimed not ours", status: "CLAIMED", wantOK: true, wantMessage: "非本服务发起"},
		{name: "claimed ours", status: "CLAIMED", own: []int{1}, wantOK: true,
			wantWrites: []string{EpCreditConfirm}, wantCredits: 1000},
		{name: "confirmed", status: "CONFIRMED", wantOK: true},
		{name: "consumed", status: "CONSUMED", wantOK: true},
		{name: "not eligible", wantOK: true},
		{name: "not yet claimable", status: "ELIGIBLE"},
		{name: "unknown status", status: "CHANGED"},
		{name: "expired", status: "ELIGIBLE", claimable: true, end: "2020-01-01T00:00:00Z", wantOK: true},
		{name: "not started", status: "ELIGIBLE", claimable: true, start: "2100-01-01T00:00:00+08:00", wantOK: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var writes []string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Agent-Type") != "PromptCenter" || r.Header.Get("X-Security-Token") != "token" || !strings.HasPrefix(r.Header.Get("Authorization"), "SDK-HMAC-SHA256 ") {
					t.Error("missing signed PromptCenter credentials")
				}
				w.Header().Set("Content-Type", "application/json")
				if r.Method == http.MethodGet {
					if r.URL.Path != EpCreditDelivery || r.URL.Query().Get("channel") != "IDE" {
						t.Errorf("wrong delivery URL: %s", r.URL)
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]any{"items": []any{
						map[string]any{"campaignId": 1, "type": "USER_LOGIN", "title": "daily", "status": tc.status, "claimable": tc.claimable, "benefitUnit": "CREDIT", "benefitAmount": 1000, "pendingTotalAmount": 1000, "extra": map[string]string{"endTime": tc.end, "startTime": tc.start}},
						map[string]any{"campaignId": 2, "type": "OTHER", "claimable": true, "status": "ELIGIBLE"},
						map[string]any{"campaignId": 3, "claimable": true, "status": "ELIGIBLE"},
					}}})
					return
				}
				writes = append(writes, r.URL.Path)
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				if body["campaignId"] != float64(1) {
					t.Errorf("unexpected campaign: %v", body)
				}
				if r.URL.Path == EpCreditClaim {
					key, _ := body["idempotentKey"].(string)
					if body["channel"] != "IDE" || !strings.HasPrefix(key, "claim_1_") {
						t.Errorf("wrong claim body: %v", body)
					}
					_, _ = fmt.Fprint(w, `{"code":0,"data":{"benefit":{"id":123}}}`)
					return
				}
				_, _ = fmt.Fprint(w, `{"code":0,"message":"ok"}`)
			}))
			defer srv.Close()
			c := New(time.Second)
			c.snapBase = srv.URL
			res, err := c.Checkin(context.Background(), SignCredential{AccessKeyID: "ak", SecretAccessKey: "sk", SecurityToken: "token"}, tc.own)
			if err != nil || res.OK != tc.wantOK || res.Credits != tc.wantCredits || !reflect.DeepEqual(writes, tc.wantWrites) {
				t.Fatalf("result=%+v err=%v writes=%v, want ok=%t credits=%g writes=%v", res, err, writes, tc.wantOK, tc.wantCredits, tc.wantWrites)
			}
			if tc.wantMessage != "" && !strings.Contains(res.Message, tc.wantMessage) {
				t.Fatalf("message = %q, want substring %q", res.Message, tc.wantMessage)
			}
		})
	}
}

// claim 应答没有可确认票据（缺 id / id:null）时不 confirm，也不算 pending。
func TestCheckinDoesNotConfirmWithoutTicket(t *testing.T) {
	for name, claim := range map[string]string{
		"missing data": `{"code":0}`,
		"null id":      `{"code":0,"data":{"benefit":{"id":null}}}`,
	} {
		t.Run(name, func(t *testing.T) {
			confirms := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case EpCreditDelivery:
					_, _ = fmt.Fprint(w, `{"code":0,"data":{"items":[{"campaignId":1,"type":"USER_LOGIN","status":"ELIGIBLE","claimable":true,"benefitAmount":500}]}}`)
				case EpCreditClaim:
					_, _ = fmt.Fprint(w, claim)
				case EpCreditConfirm:
					confirms++
					_, _ = fmt.Fprint(w, `{"code":0}`)
				}
			}))
			defer srv.Close()
			c := New(time.Second)
			c.snapBase = srv.URL
			res, err := c.Checkin(context.Background(), SignCredential{}, nil)
			if err != nil || !res.OK || confirms != 0 || len(res.Unconfirmed) != 0 || res.Credits != 500 {
				t.Fatalf("result=%+v err=%v confirms=%d", res, err, confirms)
			}
		})
	}
}

// 确认失败 → 记 pending；下一次签到只补确认它，成功后清空；次日重新变为
// ELIGIBLE 时本地不得缓存「已签」。
func TestCheckinRecoversConfirmationAndDailyReset(t *testing.T) {
	status := "ELIGIBLE"
	claimCount, confirmCount := 0, 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case EpCreditDelivery:
			_, _ = fmt.Fprintf(w, `{"code":0,"data":{"items":[{"campaignId":1,"type":"USER_LOGIN","status":%q,"claimable":true,"benefitAmount":1000,"pendingTotalAmount":1000}]}}`, status)
		case EpCreditClaim:
			claimCount++
			status = "CLAIMED"
			_, _ = fmt.Fprint(w, `{"code":0,"data":{"benefit":{"id":321}}}`)
		case EpCreditConfirm:
			confirmCount++
			if confirmCount == 1 {
				_, _ = fmt.Fprint(w, `{"code":40001,"message":"try again"}`)
				return
			}
			status = "CONFIRMED"
			_, _ = fmt.Fprint(w, `{"code":0}`)
		}
	}))
	defer srv.Close()
	c := New(time.Second)
	c.snapBase = srv.URL

	first, err := c.Checkin(context.Background(), SignCredential{}, nil)
	if err != nil || first.OK || len(first.Unconfirmed) != 1 || first.Unconfirmed[0] != 1 {
		t.Fatalf("first result=%+v err=%v", first, err)
	}
	second, err := c.Checkin(context.Background(), SignCredential{}, first.Unconfirmed)
	if err != nil || !second.OK || len(second.Unconfirmed) != 0 {
		t.Fatalf("retry result=%+v err=%v", second, err)
	}
	third, err := c.Checkin(context.Background(), SignCredential{}, second.Unconfirmed)
	if err != nil || !third.OK {
		t.Fatalf("settled result=%+v err=%v", third, err)
	}
	if claimCount != 1 || confirmCount != 2 {
		t.Fatalf("duplicate or missing write: claim=%d confirm=%d", claimCount, confirmCount)
	}

	status = "ELIGIBLE"
	reset, err := c.Checkin(context.Background(), SignCredential{}, nil)
	if err != nil || !reset.OK || reset.Credits != 1000 || claimCount != 2 || confirmCount != 3 {
		t.Fatalf("daily reset not handled: result=%+v err=%v claim=%d confirm=%d", reset, err, claimCount, confirmCount)
	}
}

// 每日签到按 type 选，不看写死的 id；新用户（4）保留为 Go 扩展。
// 每日签到只领 USER_LOGIN；新用户（4）不再混进签到流程。
func TestCheckinSelectsDailyByTypeOnly(t *testing.T) {
	var claimed []int
	confirms := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case EpCreditDelivery:
			_, _ = fmt.Fprint(w, `{"code":0,"data":{"items":[
				{"campaignId":7,"type":"USER_LOGIN","title":"daily","status":"ELIGIBLE","claimable":true,"benefitAmount":100,"pendingTotalAmount":100},
				{"campaignId":1,"type":"OTHER","status":"ELIGIBLE","claimable":true,"benefitAmount":999},
				{"campaignId":4,"type":"NEW_USER","title":"new user","status":"ELIGIBLE","claimable":true,"benefitAmount":4000,"pendingTotalAmount":4000}
			]}}`)
		case EpCreditClaim:
			var body struct {
				ID int `json:"campaignId"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			claimed = append(claimed, body.ID)
			_, _ = fmt.Fprint(w, `{"code":0,"data":{"benefit":{"id":5}}}`)
		case EpCreditConfirm:
			confirms++
			_, _ = fmt.Fprint(w, `{"code":0}`)
		}
	}))
	defer srv.Close()
	c := New(time.Second)
	c.snapBase = srv.URL
	res, err := c.Checkin(context.Background(), SignCredential{}, nil)
	if err != nil || !res.OK {
		t.Fatalf("result=%+v err=%v", res, err)
	}
	if !reflect.DeepEqual(claimed, []int{7}) || confirms != 1 || res.Credits != 100 {
		t.Fatalf("claimed=%v confirms=%d credits=%g", claimed, confirms, res.Credits)
	}
}

// 启动时的新手领取：只碰 campaign 4，已领取/无资格不报错。
func TestClaimNewUserStartupOnly(t *testing.T) {
	tests := []struct {
		name, delivery, claim string
		wantOK, wantErr       bool
		wantConfirm           int
		wantCredits           float64
		wantMessage           string
	}{
		{name: "fresh", claim: `{"code":0,"data":{"benefit":{"id":9}}}`, wantOK: true, wantConfirm: 1, wantCredits: 4000},
		{name: "without ticket", claim: `{"code":0}`, wantOK: true, wantCredits: 4000},
		{name: "already claimed", delivery: `{"code":0,"data":{"items":[{"campaignId":4,"title":"new user","status":"CONFIRMED","claimable":false,"benefitAmount":4000}]}}`, wantOK: true, wantMessage: "已领取"},
		{name: "no campaign", delivery: `{"code":0,"data":{"items":[{"campaignId":1,"type":"USER_LOGIN","status":"ELIGIBLE","claimable":true}]}}`, wantOK: true, wantMessage: "无新用户活动"},
		{name: "claim failure", claim: `{"code":40001,"message":"not ready"}`},
		{name: "delivery failure", delivery: `{"code":40001,"message":"not ready"}`, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			confirms := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case EpCreditDelivery:
					body := tc.delivery
					if body == "" {
						body = `{"code":0,"data":{"items":[{"campaignId":4,"title":"new user","status":"ELIGIBLE","claimable":true,"benefitAmount":4000,"pendingTotalAmount":4000}]}}`
					}
					_, _ = fmt.Fprint(w, body)
				case EpCreditClaim:
					var body struct {
						ID int `json:"campaignId"`
					}
					_ = json.NewDecoder(r.Body).Decode(&body)
					if body.ID != 4 {
						t.Errorf("claimed other campaign: %d", body.ID)
					}
					_, _ = fmt.Fprint(w, tc.claim)
				case EpCreditConfirm:
					confirms++
					_, _ = fmt.Fprint(w, `{"code":0}`)
				}
			}))
			defer srv.Close()
			c := New(time.Second)
			c.snapBase = srv.URL
			res, err := c.ClaimNewUser(context.Background(), SignCredential{})
			if (err != nil) != tc.wantErr || res.OK != tc.wantOK || confirms != tc.wantConfirm || res.Credits != tc.wantCredits {
				t.Fatalf("result=%+v err=%v confirms=%d", res, err, confirms)
			}
			if tc.wantMessage != "" && !strings.Contains(res.Message, tc.wantMessage) {
				t.Fatalf("message = %q, want %q", res.Message, tc.wantMessage)
			}
		})
	}
}

func TestCheckinMalformedResponses(t *testing.T) {
	tests := []struct {
		name, delivery string
		wantErr        bool
	}{
		{name: "empty campaigns", delivery: `{"code":0,"data":{"items":[]}}`},
		{name: "missing items", delivery: `{"code":0,"data":{}}`, wantErr: true},
		{name: "missing business code", delivery: `{"data":{"items":[]}}`, wantErr: true},
		{name: "delivery business failure", delivery: `{"code":40001,"message":"not ready"}`, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == EpCreditDelivery {
					_, _ = fmt.Fprint(w, tc.delivery)
					return
				}
				t.Errorf("unexpected write: %s", r.URL.Path)
			}))
			defer srv.Close()
			c := New(time.Second)
			c.snapBase = srv.URL
			if _, err := c.Checkin(context.Background(), SignCredential{}, nil); (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr=%v", err, tc.wantErr)
			}
		})
	}
}
