package pool

// 账号积分快照（statistics 桶 + 到期批次 + 每日 token 池），
// 口径对照 magpie codearts 插件：缓存 10 分钟，签到成功后强制重读。

import (
	"context"
	"fmt"
	"time"

	"codearts2api/internal/upstream"
)

// CreditsTTL 是积分快照的缓存时长。
const CreditsTTL = 10 * time.Minute

// CreditsClient 允许注入独立的积分查询客户端；默认使用各账号的上游客户端。
type CreditsClient interface {
	FetchCreditStatistics(ctx context.Context, cred upstream.SignCredential) (*upstream.CreditStatistics, error)
	FetchCreditBatches(ctx context.Context, cred upstream.SignCredential) ([]upstream.CreditBatch, error)
	FetchTokenBalance(ctx context.Context, cred upstream.SignCredential) (*upstream.TokenPool, error)
}

type creditCacheEntry struct {
	snap *upstream.CreditSnapshot
	at   time.Time
}

// AccountCredits 返回账号积分快照（缓存 CreditsTTL）。
//
// statistics 是必需来源；批次与每日 token 池读不到时快照照常返回、Error 记原因
// （签到积分的到期分析依赖批次，批次读不到时 Expiring 为空）。
func (p *Pool) AccountCredits(ctx context.Context, name string) (*upstream.CreditSnapshot, error) {
	a := p.Get(name)
	if a == nil {
		return nil, fmt.Errorf("account %s not found", name)
	}
	now := time.Now()

	p.creditsMu.Lock()
	if entry, ok := p.creditsCache[name]; ok && now.Sub(entry.at) < CreditsTTL {
		p.creditsMu.Unlock()
		return entry.snap, nil
	}
	p.creditsMu.Unlock()

	snap, err := p.fetchCredits(ctx, a)
	if err != nil {
		return nil, err
	}
	p.creditsMu.Lock()
	p.creditsCache[name] = creditCacheEntry{snap: snap, at: now}
	p.creditsMu.Unlock()
	return snap, nil
}

func (p *Pool) fetchCredits(ctx context.Context, a *Account) (*upstream.CreditSnapshot, error) {
	if _, err := p.Validate(a); err != nil {
		return nil, fmt.Errorf("validate: %w", err)
	}
	token, ak, sk := a.Auth.Credentials()
	cred := upstream.SignCredential{SecurityToken: token, AccessKeyID: ak, SecretAccessKey: sk}
	client := p.cfg.CreditsClient
	if client == nil {
		client = a.Client
	}

	stats, err := client.FetchCreditStatistics(ctx, cred)
	if err != nil {
		return nil, fmt.Errorf("credit statistics: %w", err)
	}
	batches, batchErr := client.FetchCreditBatches(ctx, cred)
	tokens, poolErr := client.FetchTokenBalance(ctx, cred)
	snap := upstream.BuildCreditSnapshot(stats, batches, tokens, time.Now())
	if batchErr != nil || poolErr != nil {
		snap.Error = joinErrs(batchErr, poolErr)
	}
	return snap, nil
}

func joinErrs(errs ...error) string {
	msg := ""
	for _, err := range errs {
		if err == nil {
			continue
		}
		if msg != "" {
			msg += "; "
		}
		msg += err.Error()
	}
	return msg
}

// InvalidateCredits 丢弃账号的积分缓存（签到成功后调用，下一次查询强制重读）。
func (p *Pool) InvalidateCredits(name string) {
	p.creditsMu.Lock()
	delete(p.creditsCache, name)
	p.creditsMu.Unlock()
}

// CachedCredits 返回缓存的快照（可能已过期，仅供面板展示）；没有则 nil。
func (p *Pool) CachedCredits(name string) *upstream.CreditSnapshot {
	p.creditsMu.Lock()
	defer p.creditsMu.Unlock()
	if entry, ok := p.creditsCache[name]; ok {
		return entry.snap
	}
	return nil
}

// cachedCreditsFor 是 List 快照里用的免锁版本（调用方已持 p.mu；creditsMu
// 与 p.mu 无反向嵌套，不会死锁）。
func (p *Pool) cachedCreditsFor(name string) *upstream.CreditSnapshot {
	if entry, ok := p.creditsCache[name]; ok {
		return entry.snap
	}
	return nil
}

// creditsTextOf 生成面板账号表里的积分摘要。
func creditsTextOf(snap *upstream.CreditSnapshot) string {
	text := ""
	for _, b := range snap.Buckets {
		if b.Name == upstream.MetricTotalCredit {
			text = fmt.Sprintf("余 %.0f", b.Remain)
			if b.Expiring > 0 {
				text += fmt.Sprintf("（%.0f 即将过期）", b.Expiring)
			}
		}
	}
	if snap.TokenPool != nil && snap.TokenPool.DailyLimit > 0 {
		if text != "" {
			text += "；"
		}
		text += fmt.Sprintf("免费额度 %.0f/%.0f", snap.TokenPool.DailyUsed, snap.TokenPool.DailyLimit)
	}
	if text == "" {
		text = "已查询"
	}
	return text
}
