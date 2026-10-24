package pool

import (
	"context"
	"errors"
	"testing"
	"time"

	"codearts2api/internal/auth"
	"codearts2api/internal/upstream"
)

type creditsClientFunc struct {
	calls      int
	batchesErr bool
	tokensErr  bool
}

func (f *creditsClientFunc) FetchCreditStatistics(ctx context.Context, cred upstream.SignCredential) (*upstream.CreditStatistics, error) {
	f.calls++
	return &upstream.CreditStatistics{
		Package: "旗舰版",
		EndDate: "2026-11-24 00:00:00",
		Buckets: []upstream.CreditBucket{
			{Name: upstream.MetricTotalCredit, Label: "总积分", Limit: 4500, Remain: 4300, Expiring: 1000},
		},
	}, nil
}

func (f *creditsClientFunc) FetchCreditBatches(ctx context.Context, cred upstream.SignCredential) ([]upstream.CreditBatch, error) {
	if f.batchesErr {
		return nil, errors.New("batches unreachable")
	}
	return []upstream.CreditBatch{
		{Metric: upstream.MetricBonusCredit, Amount: 1000, Left: 1000, ExpiresAt: time.Now().Add(24 * time.Hour).UnixMilli()},
	}, nil
}

func (f *creditsClientFunc) FetchTokenBalance(ctx context.Context, cred upstream.SignCredential) (*upstream.TokenPool, error) {
	if f.tokensErr {
		return nil, errors.New("token balance unreachable")
	}
	return &upstream.TokenPool{DailyLimit: 1000000, DailyUsed: 200000}, nil
}

func newCreditsTestPool(t *testing.T, client CreditsClient) *Pool {
	t.Helper()
	a := auth.New("u1", "user", "domain", "token", "ak", "sk", time.Now().Add(2*time.Hour).Format(time.RFC3339), "", "")
	p, err := New([]*auth.Auth{a}, Config{CreditsClient: client}, "")
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// 快照缓存 10 分钟：第二次查询不打厂商；主动失效后重读。
func TestAccountCreditsCacheAndInvalidate(t *testing.T) {
	fake := &creditsClientFunc{}
	p := newCreditsTestPool(t, fake)

	snap, err := p.AccountCredits(context.Background(), "u1")
	if err != nil {
		t.Fatalf("AccountCredits: %v", err)
	}
	if len(snap.Buckets) != 1 || snap.Buckets[0].Remain != 4300 {
		t.Fatalf("buckets = %+v", snap.Buckets)
	}
	wantReset := upstream.ParseVendorTimeString("2026-11-24 00:00:00")
	if len(snap.Batches) != 1 || snap.ResetsAt != wantReset || len(snap.Expiring) != 1 {
		t.Fatalf("派生字段缺失: %+v", snap)
	}
	if _, err := p.AccountCredits(context.Background(), "u1"); err != nil {
		t.Fatalf("第二次查询: %v", err)
	}
	if fake.calls != 1 {
		t.Fatalf("TTL 内厂商调用次数 = %d, 期望 1", fake.calls)
	}

	p.InvalidateCredits("u1")
	if _, err := p.AccountCredits(context.Background(), "u1"); err != nil {
		t.Fatalf("失效后查询: %v", err)
	}
	if fake.calls != 2 {
		t.Fatalf("失效后厂商调用次数 = %d, 期望 2", fake.calls)
	}
}

// 批次/token 池读不到：快照照常返回，Error 记原因，到期分析为空。
func TestAccountCreditsPartialFailure(t *testing.T) {
	fake := &creditsClientFunc{batchesErr: true, tokensErr: true}
	p := newCreditsTestPool(t, fake)

	snap, err := p.AccountCredits(context.Background(), "u1")
	if err != nil {
		t.Fatalf("部分失败不应整体报错: %v", err)
	}
	if snap.Error == "" {
		t.Fatal("部分失败应在 Error 里说明")
	}
	if len(snap.Buckets) != 1 || len(snap.Batches) != 0 || len(snap.Expiring) != 0 {
		t.Fatalf("部分失败快照 = %+v", snap)
	}
}

// 签到成功后积分缓存失效，下一次查询强制重读。
func TestCheckinInvalidatesCredits(t *testing.T) {
	fake := &creditsClientFunc{}
	checkin := checkinClientFunc(func(ctx context.Context, cred upstream.SignCredential) (upstream.CheckinResult, error) {
		return upstream.CheckinResult{OK: true, Message: "+1000"}, nil
	})
	a := auth.New("u1", "user", "domain", "token", "ak", "sk", time.Now().Add(2*time.Hour).Format(time.RFC3339), "", "")
	p, err := New([]*auth.Auth{a}, Config{CreditsClient: fake, CheckinClient: checkin}, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.AccountCredits(context.Background(), "u1"); err != nil {
		t.Fatalf("首次查询: %v", err)
	}
	if _, err := p.Checkin(context.Background(), "u1"); err != nil {
		t.Fatalf("签到: %v", err)
	}
	if _, err := p.AccountCredits(context.Background(), "u1"); err != nil {
		t.Fatalf("签到后查询: %v", err)
	}
	if fake.calls != 2 {
		t.Fatalf("签到后应强制重读，厂商调用次数 = %d, 期望 2", fake.calls)
	}
}
