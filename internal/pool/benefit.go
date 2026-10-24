package pool

// 每日限时福利（免费套餐）领取的状态机，照 magpie codearts 插件的
// ensureBenefitClaimed 口径：每个账号每个北京日只向厂商提交一次领取
//（官方客户端把领取做成幂等操作，打开模型菜单即调用）；领取被拒后 1 小时内
// 不再打扰厂商；厂商不可达时保持上一次状态，下一次调用立即重试。

import (
	"context"
	"errors"
	"log"
	"net/http"
	"time"

	"codearts2api/internal/upstream"
)

// BenefitClient 允许注入独立的福利领取客户端；默认使用各账号的上游客户端。
type BenefitClient interface {
	ClaimBenefit(cred upstream.SignCredential) error
}

// benefitRetryAfterRefusal 是领取被拒后多久再问厂商。
const benefitRetryAfterRefusal = time.Hour

// beijingTZ 是「北京日」的时区：签到/福利/免费池都按 UTC+8 自然日。
var beijingTZ = time.FixedZone("CST", 8*3600)

// beijingDay 返回时刻 t 的北京日（YYYY-MM-DD）。
func beijingDay(t time.Time) string {
	return t.In(beijingTZ).Format("2006-01-02")
}

type benefitDay struct {
	day string
	ok  bool
	at  time.Time
}

type benefitInflight struct {
	done chan struct{}
	ok   bool
}

// EnsureBenefitClaimed 确保账号当天已领取限时福利，返回当天是否已领取。
// 同一账号的并发调用共享同一次领取（单飞）；领取被拒记入当天状态，1 小时后
// 才重试；厂商不可达不记录状态（保持上一次结果），下次调用立即重试。
func (p *Pool) EnsureBenefitClaimed(ctx context.Context, name string) bool {
	day := beijingDay(time.Now())
	p.benefitMu.Lock()
	if st := p.benefitDays[name]; st != nil && st.day == day && (st.ok || time.Since(st.at) < benefitRetryAfterRefusal) {
		ok := st.ok
		p.benefitMu.Unlock()
		return ok
	}
	if fl := p.benefitInflight[name]; fl != nil {
		p.benefitMu.Unlock()
		<-fl.done
		return fl.ok
	}
	fl := &benefitInflight{done: make(chan struct{})}
	p.benefitInflight[name] = fl
	p.benefitMu.Unlock()

	ok, reached := p.claimBenefitOnce(ctx, name)

	p.benefitMu.Lock()
	if reached {
		p.benefitDays[name] = &benefitDay{day: day, ok: ok, at: time.Now()}
	}
	fl, inflight := p.benefitInflight[name]
	delete(p.benefitInflight, name)
	p.benefitMu.Unlock()
	if inflight && fl != nil {
		fl.ok = ok
		close(fl.done)
	}
	return ok
}

// claimBenefitOnce 向厂商提交一次领取。reached=false 表示厂商不可达
// （网络错误/5xx），此时调用方不记录新状态，保住上一次答案。
func (p *Pool) claimBenefitOnce(ctx context.Context, name string) (ok, reached bool) {
	a := p.Get(name)
	if a == nil {
		return false, true
	}
	token, ak, sk := a.Auth.Credentials()
	cred := upstream.SignCredential{SecurityToken: token, AccessKeyID: ak, SecretAccessKey: sk}
	client := p.cfg.BenefitClient
	if client == nil {
		client = a.Client
	}
	if err := client.ClaimBenefit(cred); err != nil {
		var apiErr *upstream.ApiError
		if errors.As(err, &apiErr) && apiErr.Status >= http.StatusInternalServerError {
			log.Printf("benefit claim account=%s vendor unavailable (kept last state): %v", name, err)
			return false, false
		}
		if ctx.Err() != nil {
			log.Printf("benefit claim account=%s cancelled (kept last state): %v", name, err)
			return false, false
		}
		log.Printf("benefit claim account=%s refused: %v", name, err)
		return false, true
	}
	log.Printf("benefit claim account=%s ok", name)
	return true, true
}
