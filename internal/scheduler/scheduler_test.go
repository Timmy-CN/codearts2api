package scheduler

import (
	"context"
	"errors"
	"testing"
	"time"

	"codearts2api/internal/auth"
	"codearts2api/internal/pool"
	"codearts2api/internal/upstream"
)

type failingCheckinClient struct{ calls int }

func (f *failingCheckinClient) Checkin(context.Context, upstream.SignCredential, []int) (upstream.CheckinResult, error) {
	f.calls++
	return upstream.CheckinResult{}, errors.New("temporary activity failure")
}

func TestAutoCheckinOptOutAndRetry(t *testing.T) {
	c := &failingCheckinClient{}
	a := auth.New("u1", "user", "domain", "token", "ak", "sk", time.Now().Add(2*time.Hour).Format(time.RFC3339), "", "")
	p, err := pool.New([]*auth.Auth{a}, pool.Config{CheckinClient: c}, "")
	if err != nil {
		t.Fatal(err)
	}
	New(Config{Pool: p}).Tick(context.Background())
	if c.calls != 0 {
		t.Fatal("auto claim ran while opted out")
	}
	s := New(Config{Pool: p, CreditsAutoClaim: true})
	s.Tick(context.Background())
	s.Tick(context.Background())
	if c.calls != 2 || !p.Healthy("u1") {
		t.Fatalf("retry failed or account damaged: calls=%d", c.calls)
	}
	p.Disable("u1", "manual")
	s.Tick(context.Background())
	if c.calls != 2 {
		t.Fatal("disabled account was claimed")
	}
}

// 新用户积分只在启动时领一次：Tick（30 分钟轮询）不再碰它。
func TestNewUserClaimedOnlyAtStartup(t *testing.T) {
	client := &recordingClaimClient{}
	p, err := pool.New([]*auth.Auth{
		auth.New("u1", "user", "domain", "token", "ak", "sk", time.Now().Add(2*time.Hour).Format(time.RFC3339), "", ""),
	}, pool.Config{ErrThreshold: 99, NewUserClient: client}, "")
	if err != nil {
		t.Fatal(err)
	}
	s := New(Config{Pool: p})
	s.claimNewUserOnce(context.Background())
	if client.newUsers != 1 {
		t.Fatalf("startup claim calls = %d, want 1", client.newUsers)
	}
	s.Tick(context.Background())
	if client.newUsers != 1 {
		t.Fatalf("Tick must not claim new-user credits again: calls = %d", client.newUsers)
	}
}

type recordingClaimClient struct{ checkins, newUsers int }

func (c *recordingClaimClient) Checkin(context.Context, upstream.SignCredential, []int) (upstream.CheckinResult, error) {
	c.checkins++
	return upstream.CheckinResult{OK: true}, nil
}

func (c *recordingClaimClient) ClaimNewUser(context.Context, upstream.SignCredential) (upstream.CheckinResult, error) {
	c.newUsers++
	return upstream.CheckinResult{OK: true}, nil
}
