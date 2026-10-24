package media

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"cilicili/internal/model"
	"cilicili/internal/upstream"
)

// Config 是 Manager 的构造参数。
type Config struct {
	Store     *Store
	Client    upstream.Doer
	Providers []Provider
	CoverDir  string
	Logger    *slog.Logger
	// CoverBase 是封面接口前缀，默认 /api/media；用于拼 /cover。
	CoverBase string
	// AutoConfirm 允许强匹配自动确认（默认 false，只给候选）。
	AutoConfirm bool
}

// Manager 负责聚合、元数据刮削队列、封面缓存与人工匹配。
type Manager struct {
	store     *Store
	client    upstream.Doer
	providers []Provider
	coverDir  string
	log       *slog.Logger
	coverBase string
	autoOK    bool

	mu       sync.Mutex
	wake     chan struct{}
	stop     chan struct{}
	started  bool
	cooldown map[string]time.Time
}

// NewManager 创建 Manager。
func NewManager(cfg Config) (*Manager, error) {
	if cfg.Store == nil {
		return nil, errors.New("media: Store 不能为空")
	}
	if cfg.Client == nil {
		return nil, errors.New("media: Client 不能为空")
	}
	if strings.TrimSpace(cfg.CoverDir) == "" {
		return nil, errors.New("media: CoverDir 不能为空")
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	base := strings.TrimRight(strings.TrimSpace(cfg.CoverBase), "/")
	if base == "" {
		base = "/api/media"
	}
	return &Manager{
		store:     cfg.Store,
		client:    cfg.Client,
		providers: cfg.Providers,
		coverDir:  cfg.CoverDir,
		log:       cfg.Logger,
		coverBase: base,
		autoOK:    cfg.AutoConfirm,
		wake:      make(chan struct{}, 1),
		stop:      make(chan struct{}),
		cooldown:  make(map[string]time.Time),
	}, nil
}

// Store 暴露底层存储（API 查询/统计用）。
func (m *Manager) Store() *Store { return m.store }

// UpsertItems 把搜索结果写入媒体库，返回 item 对应的 mediaID（可能为空）。
// 缺年份/类型未知的条目不会聚合成媒体实体。
func (m *Manager) UpsertItems(items []model.SearchResultItem, now time.Time) (map[string]string, error) {
	refs := make([]SourceRef, 0, len(items))
	for _, it := range items {
		refs = append(refs, SourceRef{
			SourceKey:  it.SourceKey,
			SourceName: it.SourceName,
			VodID:      it.VodID,
			Name:       it.Name,
			Pic:        it.Pic,
			TypeName:   it.TypeName,
			Year:       it.Year,
			Area:       it.Area,
			Remarks:    it.Remarks,
		})
	}
	result, err := m.store.UpsertSourceItems(refs, now)
	if err != nil {
		return nil, err
	}
	// 新出现的媒体入队刮削（已有结果/候选的不重复排）。
	for _, mediaID := range result {
		if mediaID == "" {
			continue
		}
		if err := m.store.EnqueueJob(mediaID, now); err != nil {
			return nil, err
		}
	}
	m.Notify()
	return result, nil
}

// SearchCards 把搜索结果聚合成“一张影片只出现一次”的卡片。
func (m *Manager) SearchCards(items []model.SearchResultItem, now time.Time) ([]SearchMedia, error) {
	ids, err := m.UpsertItems(items, now)
	if err != nil {
		return nil, err
	}
	type group struct {
		id   string
		refs []SourceRef
	}
	byMedia := make(map[string]*group)
	order := make([]string, 0, len(items))
	loose := make([]SearchMedia, 0)

	for _, it := range items {
		mediaID := ids[it.SourceKey+"\x00"+it.VodID]
		if mediaID == "" {
			// 不聚合的条目直接单独成卡，ID 用 sourceKey:vodId，避免与 media_ 冲突。
			loose = append(loose, SearchMedia{
				ID:          "src:" + it.SourceKey + ":" + it.VodID,
				Kind:        KindFromType(it.TypeName),
				Title:       it.Name,
				Year:        ExtractYear(it.Year),
				Cover:       it.Pic,
				TypeName:    it.TypeName,
				Remarks:     it.Remarks,
				SourceCount: 1,
				MatchStatus: MatchPending,
				Sources: []SourceRef{{
					SourceKey: it.SourceKey, SourceName: it.SourceName, VodID: it.VodID,
					Name: it.Name, Pic: it.Pic, TypeName: it.TypeName, Year: it.Year,
					Area: it.Area, Remarks: it.Remarks,
				}},
			})
			continue
		}
		g, ok := byMedia[mediaID]
		if !ok {
			g = &group{id: mediaID}
			byMedia[mediaID] = g
			order = append(order, mediaID)
		}
		g.refs = append(g.refs, SourceRef{
			SourceKey: it.SourceKey, SourceName: it.SourceName, VodID: it.VodID,
			Name: it.Name, Pic: it.Pic, TypeName: it.TypeName, Year: it.Year,
			Area: it.Area, Remarks: it.Remarks,
		})
	}

	cards := make([]SearchMedia, 0, len(order)+len(loose))
	for _, id := range order {
		g := byMedia[id]
		entity, err := m.store.GetMedia(id)
		if err != nil {
			return nil, err
		}
		if entity == nil {
			entity = &Media{ID: id, Kind: KindFromType(g.refs[0].TypeName), Title: g.refs[0].Name}
		}
		cover := entity.firstCoverURL()
		if path, err := m.store.CoverPath(id); err == nil && path != "" {
			cover = m.coverBase + "/" + id + "/cover"
		}
		cards = append(cards, SearchMedia{
			ID:          id,
			Kind:        entity.Kind,
			Title:       firstNonEmpty(entity.Metadata.Title, entity.Title),
			Year:        firstNonEmpty(entity.Metadata.Year, entity.Year),
			Cover:       cover,
			TypeName:    g.refs[0].TypeName,
			Remarks:     g.refs[0].Remarks,
			SourceCount: len(g.refs),
			MatchStatus: entity.MatchStatus,
			Sources:     g.refs,
		})
	}
	cards = append(cards, loose...)
	return cards, nil
}

func (m *Media) firstCoverURL() string {
	if m == nil {
		return ""
	}
	for _, s := range m.Sources {
		if strings.TrimSpace(s.Pic) != "" {
			return s.Pic
		}
	}
	return ""
}

// Get 读取媒体实体；本地封面存在时把 Cover.URL 指向 /api/media/{id}/cover。
func (m *Manager) Get(ctx context.Context, id string) (*Media, error) {
	media, err := m.store.GetMedia(id)
	if err != nil || media == nil {
		return nil, err
	}
	if path, err := m.store.CoverPath(id); err == nil && path != "" {
		media.Cover.URL = m.coverBase + "/" + id + "/cover"
		media.Cover.LocalPath = path
	}
	if media.Cover.URL == "" {
		media.Cover.URL = media.firstCoverURL()
	}
	if media.Metadata.Title == "" {
		media.Metadata.Title = media.Title
	}
	if media.Metadata.Year == "" {
		media.Metadata.Year = media.Year
	}
	if media.Metadata.Source == "" {
		media.Metadata.Source = "source"
	}
	return media, nil
}

// Resolve 根据采集站条目找媒体。
func (m *Manager) Resolve(ctx context.Context, sourceKey, vodID string) (*Media, error) {
	id, err := m.store.FindMediaBySource(sourceKey, vodID)
	if err != nil || id == "" {
		return nil, err
	}
	return m.Get(ctx, id)
}

// Candidates 返回候选；refresh=true 时重新排队刮削。
func (m *Manager) Candidates(ctx context.Context, id string, refresh bool) ([]Candidate, error) {
	if refresh {
		if err := m.Refresh(ctx, id); err != nil {
			return nil, err
		}
	}
	return m.store.Candidates(id)
}

// Match 人工确认/拒绝候选。provider+externalID 为空表示清除匹配。
func (m *Manager) Match(ctx context.Context, id, provider, externalID, action string) (*Media, error) {
	media, err := m.store.GetMedia(id)
	if err != nil {
		return nil, err
	}
	if media == nil {
		return nil, ErrMediaNotFound
	}
	switch action {
	case "reject":
		if err := m.store.SetMatch(id, "", "", 0, MatchRejected); err != nil {
			return nil, err
		}
	case "clear":
		if err := m.store.SetMatch(id, "", "", 0, MatchPending); err != nil {
			return nil, err
		}
	case "confirm":
		var chosen *Candidate
		for _, c := range media.Candidates {
			if c.Provider == provider && c.ExternalID == externalID {
				cc := c
				chosen = &cc
				break
			}
		}
		if chosen == nil {
			return nil, ErrCandidateNotFound
		}
		if err := m.applyCandidate(media, *chosen, MatchConfirmed); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("未知 action: %s", action)
	}
	return m.Get(ctx, id)
}

// ErrMediaNotFound / ErrCandidateNotFound 供 API 映射状态码。
var (
	ErrMediaNotFound     = errors.New("媒体不存在")
	ErrCandidateNotFound = errors.New("候选不存在")
)

// applyCandidate 把候选写入媒体元数据（采集站已有非空值不覆盖），并尝试缓存封面。
func (m *Manager) applyCandidate(media *Media, c Candidate, status MatchStatus) error {
	now := time.Now()
	media.MatchStatus = status
	media.Provider = c.Provider
	media.ExternalID = c.ExternalID
	media.Metadata.Confidence = c.Score
	if media.Metadata.Title == "" || media.Metadata.Source == "source" {
		media.Metadata.Title = c.Title
	}
	if media.Metadata.Year == "" {
		media.Metadata.Year = c.Year
	}
	if media.Metadata.Overview == "" {
		media.Metadata.Overview = c.Overview
	}
	if media.Metadata.Source == "" || media.Metadata.Source == "source" {
		media.Metadata.Source = c.Provider
	}
	if err := m.store.UpdateMetadata(media, now); err != nil {
		return err
	}
	if err := m.store.SetMatch(media.ID, c.Provider, c.ExternalID, c.Score, status); err != nil {
		return err
	}
	// 封面：本地已有就不动；否则尝试候选封面。
	if path, _ := m.store.CoverPath(media.ID); path == "" && c.CoverURL != "" {
		ci, err := m.downloadCover(context.Background(), media.ID, c.CoverURL, c.Provider)
		if err == nil {
			_ = m.store.SaveCover(media.ID, ci, now)
		}
	}
	return nil
}

// SetCover 人工设置封面 URL，立即下载缓存；失败返回错误。
func (m *Manager) SetCover(ctx context.Context, id, rawURL string) (*Media, error) {
	media, err := m.store.GetMedia(id)
	if err != nil {
		return nil, err
	}
	if media == nil {
		return nil, ErrMediaNotFound
	}
	ci, err := m.downloadCover(ctx, id, rawURL, "manual")
	if err != nil {
		return nil, err
	}
	ci.UpdatedAt = time.Now().UnixMilli()
	if err := m.store.SaveCover(id, ci, time.Now()); err != nil {
		return nil, err
	}
	return m.Get(ctx, id)
}

// CoverPath 返回本地封面路径。
func (m *Manager) CoverPath(id string) (string, error) {
	return m.store.CoverPath(id)
}

// Refresh 重置刮削状态并排队。
func (m *Manager) Refresh(ctx context.Context, id string) error {
	if err := m.store.SetMetadataStatus(id, MetaQueued); err != nil {
		return err
	}
	if err := m.store.EnqueueJob(id, time.Now()); err != nil {
		return err
	}
	m.Notify()
	return nil
}

// firstNonEmpty 返回第一个非空字符串。
func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// ensureCover 按优先级为媒体准备本地封面：
// 已有本地文件 → 采集源封面 → 候选封面。返回是否落盘成功。
func (m *Manager) ensureCover(ctx context.Context, media *Media, candidates []Candidate) bool {
	if path, err := m.store.CoverPath(media.ID); err == nil && path != "" {
		return true
	}
	// 采集源封面：按源 key 排序保证稳定。
	refs := append([]SourceRef(nil), media.Sources...)
	sort.SliceStable(refs, func(i, j int) bool { return refs[i].SourceKey < refs[j].SourceKey })
	for _, ref := range refs {
		if strings.TrimSpace(ref.Pic) == "" {
			continue
		}
		ci, err := m.downloadCover(ctx, media.ID, ref.Pic, ref.SourceKey)
		if err == nil {
			ci.UpdatedAt = time.Now().UnixMilli()
			if err := m.store.SaveCover(media.ID, ci, time.Now()); err == nil {
				return true
			}
		}
	}
	// 候选封面（已排序）。
	for _, c := range candidates {
		if strings.TrimSpace(c.CoverURL) == "" {
			continue
		}
		ci, err := m.downloadCover(ctx, media.ID, c.CoverURL, c.Provider)
		if err == nil {
			ci.UpdatedAt = time.Now().UnixMilli()
			if err := m.store.SaveCover(media.ID, ci, time.Now()); err == nil {
				return true
			}
		}
	}
	// 全部失败：记录不可达（不阻塞后续）。
	return false
}

// RemoveCover 删除本地封面文件与记录（人工重设前用）。
func (m *Manager) RemoveCover(id string) error {
	if path, _ := m.store.CoverPath(id); path != "" {
		_ = os.Remove(path)
	}
	_, err := m.store.db.Exec(`DELETE FROM cover_cache WHERE media_id=?`, id)
	return err
}
