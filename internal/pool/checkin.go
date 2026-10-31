package pool

import (
	"context"
	"fmt"
	"time"

	"codearts2api/internal/upstream"
)

// CheckinClient 允许注入独立的活动客户端；默认使用各账号的上游客户端。
type CheckinClient interface {
	Checkin(context.Context, upstream.SignCredential, []int) (upstream.CheckinResult, error)
}

type CheckinStatus struct {
	OK        bool      `json:"ok"`
	Message   string    `json:"message"`
	CheckedAt time.Time `json:"checked_at"`
}

// Checkin 串行领取指定账号的每日签到和新手福利。活动故障只记录签到结果，
// 不计入推理错误、冷却或禁用；token 仍由原有 Validate/RefreshToken 处理。
func (p *Pool) Checkin(ctx context.Context, name string) (upstream.CheckinResult, error) {
	a := p.Get(name)
	if a == nil {
		return upstream.CheckinResult{}, fmt.Errorf("account not found")
	}
	a.checkinMu.Lock()
	defer a.checkinMu.Unlock()
	if err := ctx.Err(); err != nil {
		return upstream.CheckinResult{}, err
	}
	a.mu.Lock()
	disabled := a.disabled
	a.mu.Unlock()
	if disabled {
		return upstream.CheckinResult{}, fmt.Errorf("account disabled")
	}
	ok, err := p.Validate(a)
	if err != nil || !ok {
		if err == nil {
			err = fmt.Errorf("token invalid; re-login required")
		}
		return upstream.CheckinResult{}, err
	}
	token, ak, sk := a.Auth.Credentials()
	client := p.cfg.CheckinClient
	if client == nil {
		client = a.Client
	}
	today := time.Now().Format("2006-01-02")
	a.mu.Lock()
	own := append([]int(nil), a.checkinPending...)
	if a.checkinPendingDay != today {
		own = nil // 跨天丢弃：昨天没确认的 claim 不再补
	}
	a.mu.Unlock()

	result, err := client.Checkin(ctx, upstream.SignCredential{SecurityToken: token, AccessKeyID: ak, SecretAccessKey: sk}, own)
	if err != nil {
		result.OK, result.Message = false, err.Error()
	}
	a.mu.Lock()
	a.checkin = &CheckinStatus{OK: result.OK, Message: result.Message, CheckedAt: time.Now()}
	if err == nil {
		a.checkinPending = append([]int(nil), result.Unconfirmed...)
		a.checkinPendingDay = ""
		if len(result.Unconfirmed) > 0 {
			a.checkinPendingDay = today
		}
	}
	a.mu.Unlock()
	// 签到成功会入账新积分：丢掉积分缓存，下一次查询强制重读（插件同口径）。
	if err == nil && result.OK {
		p.InvalidateCredits(name)
	}
	return result, err
}
