package pool

import (
	"context"
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"codearts2api/internal/auth"
	"codearts2api/internal/upstream"
)

type benefitClientFunc func(cred upstream.SignCredential) error

func (f benefitClientFunc) ClaimBenefit(cred upstream.SignCredential) error { return f(cred) }

func newBenefitTestPool(t *testing.T, client BenefitClient) *Pool {
	t.Helper()
	a := auth.New("u1", "user", "domain", "token", "ak", "sk", time.Now().Add(2*time.Hour).Format(time.RFC3339), "", "")
	p, err := New([]*auth.Auth{a}, Config{BenefitClient: client}, "")
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// 当天领取成功后不再打扰厂商；结果被记住。
func TestBenefitClaimOncePerDay(t *testing.T) {
	var calls atomic.Int32
	p := newBenefitTestPool(t, benefitClientFunc(func(cred upstream.SignCredential) error {
		calls.Add(1)
		return nil
	}))
	if !p.EnsureBenefitClaimed(context.Background(), "u1") {
		t.Fatal("首次领取应成功")
	}
	if !p.EnsureBenefitClaimed(context.Background(), "u1") {
		t.Fatal("当天第二次应直接返回已领取")
	}
	if calls.Load() != 1 {
		t.Fatalf("厂商调用次数 = %d, 期望 1", calls.Load())
	}
}

// 领取被拒记入当天状态，1 小时内不重试厂商。
func TestBenefitClaimRefusalBacksOff(t *testing.T) {
	var calls atomic.Int32
	p := newBenefitTestPool(t, benefitClientFunc(func(cred upstream.SignCredential) error {
		calls.Add(1)
		return fmt.Errorf("claim failed: error_code=4004 msg=benefit not found")
	}))
	if p.EnsureBenefitClaimed(context.Background(), "u1") {
		t.Fatal("被拒应返回 false")
	}
	if p.EnsureBenefitClaimed(context.Background(), "u1") {
		t.Fatal("退避期内应返回 false")
	}
	if calls.Load() != 1 {
		t.Fatalf("退避期内厂商调用次数 = %d, 期望 1", calls.Load())
	}
}

// 厂商 5xx/不可达：不记录当天状态，下一次调用立即重试。
func TestBenefitClaimTransientRetriesImmediately(t *testing.T) {
	var calls atomic.Int32
	fail := atomic.Bool{}
	fail.Store(true)
	p := newBenefitTestPool(t, benefitClientFunc(func(cred upstream.SignCredential) error {
		calls.Add(1)
		if fail.Load() {
			return &upstream.ApiError{Code: http.StatusBadGateway, Status: http.StatusBadGateway, Message: "gateway down"}
		}
		return nil
	}))
	if p.EnsureBenefitClaimed(context.Background(), "u1") {
		t.Fatal("厂商不可达应返回 false")
	}
	fail.Store(false)
	if !p.EnsureBenefitClaimed(context.Background(), "u1") {
		t.Fatal("恢复后应重试并成功")
	}
	if calls.Load() != 2 {
		t.Fatalf("厂商调用次数 = %d, 期望 2", calls.Load())
	}
}
