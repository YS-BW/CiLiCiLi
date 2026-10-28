package media

import (
	"context"
	"errors"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// 自动确认阈值：标题归一化相等 + 年份相等 + 综合分足够高。
const autoConfirmScore = 0.85

// Notify 唤醒后台刮削 worker（非阻塞）。
func (m *Manager) Notify() {
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

// Start 启动后台刮削 worker。可重复调用，只会启动一次。
func (m *Manager) Start(ctx context.Context) {
	m.mu.Lock()
	if m.started {
		m.mu.Unlock()
		return
	}
	m.started = true
	m.mu.Unlock()
	go m.loop(ctx)
}

// Stop 停止 worker。
func (m *Manager) Stop() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.started {
		return
	}
	close(m.stop)
	m.started = false
}

// scrapeConcurrency 是刮削 worker 的并发度：搜索一次可能产生几十个媒体，
// 串行处理会让卡片长时间停留在源站图，故并发拉 TMDB/源站封面与元数据。
const scrapeConcurrency = 6

func (m *Manager) loop(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	sem := make(chan struct{}, scrapeConcurrency)
	var wg sync.WaitGroup
	// drain：持续 claim 直到队列空；每个 job 在独立 goroutine 中并发执行（受 sem 限流）。
	drain := func() {
		for {
			id, attempts, ok, err := m.store.ClaimJob(time.Now())
			if err != nil {
				m.log.Warn("claim scrape job failed", "err", err)
				return
			}
			if !ok {
				return
			}
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				return
			}
			wg.Add(1)
			go func(mediaID string, att int) {
				defer wg.Done()
				defer func() { <-sem }()
				m.runJob(ctx, mediaID, att)
			}(id, attempts)
		}
	}
	for {
		drain()
		select {
		case <-ctx.Done():
			wg.Wait()
			return
		case <-m.stop:
			wg.Wait()
			return
		case <-m.wake:
		case <-ticker.C:
		}
	}
}

// runJob 处理一个媒体：调用所有 provider、保存候选、尝试自动确认、准备封面。
func (m *Manager) runJob(ctx context.Context, mediaID string, attempts int) {
	now := time.Now()
	fail := func(err error) {
		retry := now.Add(backoffFor(attempts + 1))
		_ = m.store.SetMetadataStatus(mediaID, MetaFailed)
		_ = m.store.FinishJob(mediaID, truncateErr(err), retry, now)
		m.log.Warn("scrape failed", "media", mediaID, "attempts", attempts+1, "err", err, "retryAt", retry)
	}
	media, err := m.store.GetMedia(mediaID)
	if err != nil || media == nil {
		if err == nil {
			err = ErrMediaNotFound
		}
		fail(err)
		return
	}
	if err := m.store.SetMetadataStatus(mediaID, MetaScraping); err != nil {
		fail(err)
		return
	}

	runCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	req := Request{Kind: media.Kind, Title: media.Title, Year: media.Year}

	candidates := make([]Candidate, 0, 16)
	var providerErrs []string
	attempted := 0
	for _, p := range m.providers {
		if p == nil || !p.Enabled() {
			continue
		}
		if !m.providerAvailable(p.Name(), now) {
			continue
		}
		attempted++
		cs, err := p.Search(runCtx, req)
		if err != nil {
			m.markProviderFailure(p.Name(), now)
			providerErrs = append(providerErrs, p.Name()+": "+err.Error())
			m.log.Debug("provider failed", "provider", p.Name(), "media", mediaID, "err", err)
			continue
		}
		m.markProviderSuccess(p.Name())
		for i := range cs {
			cs[i].Score, cs[i].Reasons = ScoreCandidate(req, cs[i])
		}
		candidates = append(candidates, cs...)
	}
	sort.SliceStable(candidates, func(i, j int) bool { return candidates[i].Score > candidates[j].Score })

	if err := m.store.SaveCandidates(mediaID, candidates, now); err != nil {
		fail(err)
		return
	}

	best := Candidate{}
	if len(candidates) > 0 {
		best = candidates[0]
	}
	switch {
	case len(candidates) == 0:
		// 没有候选：保持 pending，但采集站封面仍然要缓存。
		_ = m.store.SetMatch(mediaID, "", "", 0, MatchPending)
	case m.autoOK && isStrongMatch(req, best):
		media.Candidates = candidates
		if err := m.applyCandidate(media, best, MatchConfirmed); err != nil {
			fail(err)
			return
		}
	default:
		_ = m.store.SetMatch(mediaID, best.Provider, best.ExternalID, best.Score, MatchSuggested)
	}
	// 封面优先采集源，其次候选。
	if refreshed, err := m.store.GetMedia(mediaID); err == nil && refreshed != nil {
		m.ensureCover(runCtx, refreshed, candidates)
	}
	if err := m.store.SetMetadataStatus(mediaID, MetaDone); err != nil {
		fail(err)
		return
	}
	// 有 provider 全部失败且没有任何候选时，记一次失败并退避。
	if len(candidates) == 0 && attempted > 0 && len(providerErrs) == attempted {
		fail(errors.New(strings.Join(providerErrs, "; ")))
		return
	}
	if err := m.store.FinishJob(mediaID, "", time.Time{}, now); err != nil {
		m.log.Warn("finish scrape job failed", "media", mediaID, "err", err)
	}
}

// providerCooldown 是 provider 连续失败后的冷却时长。
const providerCooldown = 5 * time.Minute

// providerAvailable 表示 provider 是否已过冷却期。
func (m *Manager) providerAvailable(name string, now time.Time) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	until, ok := m.cooldown[name]
	return !ok || !now.Before(until)
}

// markProviderFailure 让不可达的 provider 冷却一段时间，避免每个媒体都等一次超时。
func (m *Manager) markProviderFailure(name string, now time.Time) {
	m.mu.Lock()
	m.cooldown[name] = now.Add(providerCooldown)
	m.mu.Unlock()
}

// markProviderSuccess 清除冷却。
func (m *Manager) markProviderSuccess(name string) {
	m.mu.Lock()
	delete(m.cooldown, name)
	m.mu.Unlock()
}

// isStrongMatch 只在标题归一化相等、年份一致时允许自动确认。
func isStrongMatch(req Request, c Candidate) bool {
	score, _ := ScoreCandidate(req, c)
	if score < autoConfirmScore {
		return false
	}
	wantBase, wantSeason := NormalizeTitle(req.Title)
	gotBase, gotSeason := NormalizeTitle(c.Title)
	if wantBase == "" || wantBase != gotBase {
		return false
	}
	// 季数必须一致；没有季数信息时（如电影）按 0 处理。
	if wantSeason != gotSeason {
		return false
	}
	if req.Year == "" || c.Year == "" || req.Year != c.Year {
		return false
	}
	return true
}

// ScoreCandidate 统一候选打分，返回分值与命中的原因。
func ScoreCandidate(req Request, c Candidate) (float64, []string) {
	score := 0.0
	reasons := make([]string, 0, 4)

	want, _ := NormalizeTitle(req.Title)
	got, _ := NormalizeTitle(c.Title)
	switch {
	case want == "" || got == "":
	case want == got:
		score += 0.55
		reasons = append(reasons, "title exact")
	case strings.Contains(got, want) || strings.Contains(want, got):
		score += 0.3
		reasons = append(reasons, "title partial")
	}

	if req.Year != "" && c.Year != "" {
		if req.Year == c.Year {
			score += 0.3
			reasons = append(reasons, "year exact")
		} else if absYearDiff(req.Year, c.Year) == 1 {
			score += 0.1
			reasons = append(reasons, "year ±1")
		} else {
			score -= 0.15
			reasons = append(reasons, "year mismatch")
		}
	}

	if c.Kind == req.Kind {
		score += 0.1
		reasons = append(reasons, "kind match")
	} else if (req.Kind == KindAnime && c.Kind == KindTV) || (req.Kind == KindTV && c.Kind == KindAnime) {
		score += 0.05
		reasons = append(reasons, "kind compatible")
	} else if req.Kind != KindOther && c.Kind != KindOther {
		score -= 0.2
		reasons = append(reasons, "kind mismatch")
	}

	if req.Kind == KindAnime && c.Provider == "bangumi" {
		score += 0.05
		reasons = append(reasons, "anime provider")
	}
	if score < 0 {
		score = 0
	}
	if score > 1 {
		score = 1
	}
	return score, reasons
}

func absYearDiff(a, b string) int {
	// 注意：不能复用 atoiSafe（它对 >999 返回 0，是季数解析器，会把 4 位年份当非法）。
	ai, e1 := strconv.Atoi(strings.TrimSpace(a))
	bi, e2 := strconv.Atoi(strings.TrimSpace(b))
	if e1 != nil || e2 != nil || ai == 0 || bi == 0 {
		return 999
	}
	if ai > bi {
		return ai - bi
	}
	return bi - ai
}

// backoffFor 返回第 n 次失败后的重试间隔：5m、10m、20m… 上限 6h。
func backoffFor(attempts int) time.Duration {
	if attempts < 1 {
		attempts = 1
	}
	d := 5 * time.Minute
	for i := 1; i < attempts && d < 6*time.Hour; i++ {
		d *= 2
	}
	if d > 6*time.Hour {
		d = 6 * time.Hour
	}
	return d
}

func truncateErr(err error) string {
	if err == nil {
		return ""
	}
	s := err.Error()
	if len([]rune(s)) > 500 {
		s = string([]rune(s)[:500])
	}
	return s
}
