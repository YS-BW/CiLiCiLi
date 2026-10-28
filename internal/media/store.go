package media

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// SchemaVersion 用于将来迁移。
const SchemaVersion = 1

// Store 是媒体库存储（SQLite + WAL）。
type Store struct {
	db  *sql.DB
	now func() time.Time
}

const schemaSQL = `
CREATE TABLE IF NOT EXISTS media (
  id              TEXT PRIMARY KEY,
  kind            TEXT NOT NULL,
  title           TEXT NOT NULL,
  year            TEXT NOT NULL DEFAULT '',
  normalized      TEXT NOT NULL DEFAULT '',
  season          INTEGER NOT NULL DEFAULT 0,
  meta_title      TEXT NOT NULL DEFAULT '',
  meta_year       TEXT NOT NULL DEFAULT '',
  meta_overview   TEXT NOT NULL DEFAULT '',
  meta_cover      TEXT NOT NULL DEFAULT '',
  meta_cover_src  TEXT NOT NULL DEFAULT '',
  meta_backdrop   TEXT NOT NULL DEFAULT '',
  meta_director   TEXT NOT NULL DEFAULT '',
  meta_actors     TEXT NOT NULL DEFAULT '',
  meta_area       TEXT NOT NULL DEFAULT '',
  meta_genres     TEXT NOT NULL DEFAULT '',
  meta_source     TEXT NOT NULL DEFAULT '',
  metadata_status TEXT NOT NULL DEFAULT 'idle',
  match_status    TEXT NOT NULL DEFAULT 'pending',
  provider        TEXT NOT NULL DEFAULT '',
  external_id     TEXT NOT NULL DEFAULT '',
  confidence      REAL NOT NULL DEFAULT 0,
  updated_at      INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS source_items (
  source_key TEXT NOT NULL,
  vod_id     TEXT NOT NULL,
  media_id   TEXT NOT NULL,
  name       TEXT NOT NULL DEFAULT '',
  pic        TEXT NOT NULL DEFAULT '',
  type_name  TEXT NOT NULL DEFAULT '',
  year       TEXT NOT NULL DEFAULT '',
  area       TEXT NOT NULL DEFAULT '',
  remarks    TEXT NOT NULL DEFAULT '',
  source_name TEXT NOT NULL DEFAULT '',
  updated_at INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (source_key, vod_id)
);
CREATE INDEX IF NOT EXISTS idx_source_items_media ON source_items(media_id);

CREATE TABLE IF NOT EXISTS metadata_candidates (
  media_id    TEXT NOT NULL,
  provider    TEXT NOT NULL,
  external_id TEXT NOT NULL,
  payload     TEXT NOT NULL,
  score       REAL NOT NULL DEFAULT 0,
  updated_at  INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (media_id, provider, external_id)
);

CREATE TABLE IF NOT EXISTS cover_cache (
  media_id   TEXT PRIMARY KEY,
  url        TEXT NOT NULL DEFAULT '',
  local_path TEXT NOT NULL DEFAULT '',
  hash       TEXT NOT NULL DEFAULT '',
  width      INTEGER NOT NULL DEFAULT 0,
  height     INTEGER NOT NULL DEFAULT 0,
  source     TEXT NOT NULL DEFAULT '',
  reachable  INTEGER NOT NULL DEFAULT 0,
  updated_at INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS scrape_jobs (
  media_id    TEXT NOT NULL,
  provider    TEXT NOT NULL,
  status      TEXT NOT NULL DEFAULT 'queued',
  attempts    INTEGER NOT NULL DEFAULT 0,
  last_error  TEXT NOT NULL DEFAULT '',
  next_run_at INTEGER NOT NULL DEFAULT 0,
  updated_at  INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (media_id, provider)
);
`

// NewStore 打开/创建媒体库。
func NewStore(path string) (*Store, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("媒体库路径不能为空")
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return nil, fmt.Errorf("创建媒体库目录: %w", err)
		}
	}
	dsn := path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("打开媒体库: %w", err)
	}
	db.SetMaxOpenConns(4)
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("连接媒体库: %w", err)
	}
	if _, err := db.Exec(schemaSQL); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("初始化媒体库: %w", err)
	}
	// 轻量迁移：老库没有 meta_backdrop 列时补上（只增不改，重复执行安全）。
	if err := ensureColumn(db, "media", "meta_backdrop", "TEXT NOT NULL DEFAULT ''"); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("迁移媒体库 meta_backdrop: %w", err)
	}
	// 统一封面存储表（大/小封面按 key+variant 落库）。
	if _, err := db.Exec(coverStoreSchema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("初始化封面存储表: %w", err)
	}
	s := &Store{db: db, now: time.Now}
	if err := s.recoverInterrupted(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("恢复媒体库任务: %w", err)
	}
	return s, nil
}

// ensureColumn 在列不存在时对表执行 ALTER TABLE ADD COLUMN（SQLite 无 IF NOT EXISTS）。
func ensureColumn(db *sql.DB, table, column, ddl string) error {
	rows, err := db.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return err
		}
		if name == column {
			return nil
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	_, err = db.Exec(`ALTER TABLE ` + table + ` ADD COLUMN ` + column + ` ` + ddl)
	return err
}

// Close 关闭数据库。
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

// GetMedia 读取一个聚合实体（含源引用、候选、封面）。
func (s *Store) GetMedia(id string) (*Media, error) {
	row := s.db.QueryRow(`SELECT id, kind, title, year, meta_title, meta_year, meta_overview,
		meta_cover, meta_cover_src, meta_backdrop, meta_director, meta_actors, meta_area, meta_genres, meta_source,
		metadata_status, match_status, confidence, updated_at FROM media WHERE id = ?`, id)
	m := &Media{}
	var (
		metaSource string
		coverSrc   string
	)
	err := row.Scan(&m.ID, &m.Kind, &m.Title, &m.Year, &m.Metadata.Title, &m.Metadata.Year,
		&m.Metadata.Overview, &m.Metadata.Cover, &coverSrc, &m.Metadata.Backdrop, &m.Metadata.Director, &m.Metadata.Actors,
		&m.Metadata.Area, &m.Metadata.Genres, &metaSource, &m.MetadataStatus, &m.MatchStatus,
		&m.Metadata.Confidence, &m.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	m.Metadata.Source = metaSource
	m.Backdrop = m.Metadata.Backdrop
	// provider/external_id 在 GetMedia 的扫描里未取，这里单独补上。
	_ = s.db.QueryRow(`SELECT provider, external_id FROM media WHERE id=?`, id).Scan(&m.Provider, &m.ExternalID)
	m.Cover.URL = m.Metadata.Cover
	m.Cover.Source = coverSrc
	sources, err := s.listSources(id)
	if err != nil {
		return nil, err
	}
	m.Sources = sources
	m.SourceCount = len(sources)
	m.Candidates, err = s.listCandidates(id)
	if err != nil {
		return nil, err
	}
	if ci, err := s.getCover(id); err != nil {
		return nil, err
	} else if ci != nil {
		m.Cover = *ci
	}
	return m, nil
}

// ListMediaByIDs 批量读取，用于搜索结果聚合。
func (s *Store) ListMediaByIDs(ids []string) (map[string]*Media, error) {
	out := make(map[string]*Media, len(ids))
	for _, id := range ids {
		m, err := s.GetMedia(id)
		if err != nil {
			return nil, err
		}
		if m != nil {
			out[id] = m
		}
	}
	return out, nil
}

func (s *Store) listSources(mediaID string) ([]SourceRef, error) {
	rows, err := s.db.Query(`SELECT source_key, source_name, vod_id, name, pic, type_name, year, area, remarks
		FROM source_items WHERE media_id = ? ORDER BY source_key`, mediaID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]SourceRef, 0)
	for rows.Next() {
		var r SourceRef
		if err := rows.Scan(&r.SourceKey, &r.SourceName, &r.VodID, &r.Name, &r.Pic, &r.TypeName, &r.Year, &r.Area, &r.Remarks); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) listCandidates(mediaID string) ([]Candidate, error) {
	rows, err := s.db.Query(`SELECT provider, external_id, payload, score, updated_at
		FROM metadata_candidates WHERE media_id = ? ORDER BY score DESC`, mediaID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Candidate, 0)
	for rows.Next() {
		var (
			provider, externalID, payload string
			score                         float64
			updatedAt                     int64
		)
		if err := rows.Scan(&provider, &externalID, &payload, &score, &updatedAt); err != nil {
			return nil, err
		}
		var c Candidate
		if err := json.Unmarshal([]byte(payload), &c); err != nil {
			continue
		}
		c.Provider = provider
		c.ExternalID = externalID
		c.Score = score
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Store) getCover(mediaID string) (*CoverInfo, error) {
	row := s.db.QueryRow(`SELECT url, local_path, hash, width, height, source, reachable, updated_at
		FROM cover_cache WHERE media_id = ?`, mediaID)
	var (
		ci        CoverInfo
		reachable int
	)
	err := row.Scan(&ci.URL, &ci.LocalPath, &ci.Hash, &ci.Width, &ci.Height, &ci.Source, &reachable, &ci.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	b := reachable != 0
	ci.Reachable = &b
	return &ci, nil
}

// UpsertSourceItems 把采集站条目写入媒体库。返回受影响（新建或更新）的媒体 ID 集合。
// 聚合键为空（缺年份/类型未知）时不聚合，返回空串。
func (s *Store) UpsertSourceItems(refs []SourceRef, now time.Time) (map[string]string, error) {
	mediaIDs := make(map[string]string, len(refs))
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	for _, ref := range refs {
		kind := KindFromType(ref.TypeName)
		year := ExtractYear(ref.Year)
		mediaID := AggregateKey(kind, ref.Name, year)
		mediaIDs[ref.SourceKey+"\x00"+ref.VodID] = mediaID
		if mediaID == "" {
			continue
		}
		base, season := NormalizeTitle(ref.Name)
		// 先确保 media 行存在：标题/年份以第一次出现为准，后续不覆盖采集站标题。
		if _, err := tx.Exec(`INSERT INTO media (id, kind, title, year, normalized, season, meta_title, meta_year,
			meta_source, metadata_status, match_status, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, '', '', 'source', 'idle', 'pending', ?)
			ON CONFLICT(id) DO NOTHING`,
			mediaID, kind, ref.Name, year, base, season, now.UnixMilli()); err != nil {
			return nil, err
		}
		if _, err := tx.Exec(`INSERT INTO source_items (source_key, vod_id, media_id, name, pic, type_name, year, area, remarks, source_name, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(source_key, vod_id) DO UPDATE SET media_id=excluded.media_id, name=excluded.name,
			pic=excluded.pic, type_name=excluded.type_name, year=excluded.year, area=excluded.area,
			remarks=excluded.remarks, source_name=excluded.source_name, updated_at=excluded.updated_at`,
			ref.SourceKey, ref.VodID, mediaID, ref.Name, ref.Pic, ref.TypeName, year, ref.Area, ref.Remarks, ref.SourceName, now.UnixMilli()); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return mediaIDs, nil
}

// UpdateMetadata 写入已确认/选中的第三方元数据（不覆盖采集站已有非空标题）。
func (s *Store) UpdateMetadata(m *Media, now time.Time) error {
	_, err := s.db.Exec(`UPDATE media SET meta_title=?, meta_year=?, meta_overview=?, meta_cover=?,
		meta_cover_src=?, meta_backdrop=?, meta_director=?, meta_actors=?, meta_area=?, meta_genres=?, meta_source=?,
		metadata_status=?, match_status=?, provider=?, external_id=?, confidence=?, updated_at=? WHERE id=?`,
		m.Metadata.Title, m.Metadata.Year, m.Metadata.Overview, m.Metadata.Cover, m.Cover.Source,
		m.Metadata.Backdrop,
		m.Metadata.Director, m.Metadata.Actors, m.Metadata.Area, m.Metadata.Genres, m.Metadata.Source,
		m.MetadataStatus, m.MatchStatus, m.Provider, m.ExternalID, m.Metadata.Confidence,
		now.UnixMilli(), m.ID)
	return err
}

// SetStatuses 只更新状态字段（排队/抓取中/失败）。
func (s *Store) SetStatuses(id string, meta MetadataStatus, match MatchStatus, now time.Time) error {
	_, err := s.db.Exec(`UPDATE media SET metadata_status=?, match_status=?, updated_at=? WHERE id=?`,
		meta, match, now.UnixMilli(), id)
	return err
}

// SaveCandidates 替换某媒体的候选列表。
func (s *Store) SaveCandidates(mediaID string, candidates []Candidate, now time.Time) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM metadata_candidates WHERE media_id=?`, mediaID); err != nil {
		return err
	}
	for _, c := range candidates {
		raw, err := json.Marshal(c)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO metadata_candidates (media_id, provider, external_id, payload, score, updated_at)
			VALUES (?, ?, ?, ?, ?, ?)`, mediaID, c.Provider, c.ExternalID, string(raw), c.Score, now.UnixMilli()); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// SaveCover 写入本地封面缓存记录。
func (s *Store) SaveCover(mediaID string, ci CoverInfo, now time.Time) error {
	reachable := 0
	if ci.Reachable != nil && *ci.Reachable {
		reachable = 1
	}
	_, err := s.db.Exec(`INSERT INTO cover_cache (media_id, url, local_path, hash, width, height, source, reachable, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(media_id) DO UPDATE SET url=excluded.url, local_path=excluded.local_path,
		hash=excluded.hash, width=excluded.width, height=excluded.height, source=excluded.source,
		reachable=excluded.reachable, updated_at=excluded.updated_at`,
		mediaID, ci.URL, ci.LocalPath, ci.Hash, ci.Width, ci.Height, ci.Source, reachable, now.UnixMilli())
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`UPDATE media SET meta_cover=?, meta_cover_src=?, updated_at=? WHERE id=?`,
		ci.URL, ci.Source, now.UnixMilli(), mediaID)
	return err
}

// MediaRefs 返回媒体下所有源引用（用于详情/换源）。
func (s *Store) MediaRefs(mediaID string) ([]SourceRef, error) {
	return s.listSources(mediaID)
}

// FindMediaBySource 根据 sourceKey+vodId 找媒体 ID。
func (s *Store) FindMediaBySource(sourceKey, vodID string) (string, error) {
	var id string
	err := s.db.QueryRow(`SELECT media_id FROM source_items WHERE source_key=? AND vod_id=?`, sourceKey, vodID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return id, err
}

// Stats 返回媒体库计数，便于 /api/status 与调试。
func (s *Store) Stats() (map[string]int, error) {
	out := map[string]int{}
	for _, q := range []struct {
		name string
		sql  string
	}{
		{"media", `SELECT COUNT(*) FROM media`},
		{"sourceItems", `SELECT COUNT(*) FROM source_items`},
		{"candidates", `SELECT COUNT(*) FROM metadata_candidates`},
		{"covers", `SELECT COUNT(*) FROM cover_cache`},
		{"jobs", `SELECT COUNT(*) FROM scrape_jobs`},
	} {
		var n int
		if err := s.db.QueryRow(q.sql).Scan(&n); err != nil {
			return nil, err
		}
		out[q.name] = n
	}
	return out, nil
}

// PendingMedia 是后台刮削需要的媒体快照。
type PendingMedia struct {
	ID    string
	Kind  Kind
	Title string
	Year  string
}

// Pending 返回待刮削的媒体。
func (s *Store) Pending(limit int) ([]PendingMedia, error) {
	return s.listPending(limit)
}

func (s *Store) listPending(limit int) ([]PendingMedia, error) {
	rows, err := s.db.Query(`SELECT id, kind, title, year FROM media
		WHERE metadata_status IN ('idle','failed') AND match_status IN ('pending','suggested')
		ORDER BY updated_at ASC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]PendingMedia, 0)
	for rows.Next() {
		var p PendingMedia
		if err := rows.Scan(&p.ID, &p.Kind, &p.Title, &p.Year); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// Candidates 返回某媒体的候选。
func (s *Store) Candidates(mediaID string) ([]Candidate, error) {
	return s.listCandidates(mediaID)
}

// SetMatch 写入人工/自动匹配结果。
func (s *Store) SetMatch(mediaID, provider, externalID string, confidence float64, status MatchStatus) error {
	_, err := s.db.Exec(`UPDATE media SET provider=?, external_id=?, confidence=?, match_status=?, updated_at=?
		WHERE id=?`, provider, externalID, confidence, status, s.now().UnixMilli(), mediaID)
	return err
}

// SetMetadataStatus 只更新刮削状态。
func (s *Store) SetMetadataStatus(mediaID string, status MetadataStatus) error {
	_, err := s.db.Exec(`UPDATE media SET metadata_status=?, updated_at=? WHERE id=?`,
		status, s.now().UnixMilli(), mediaID)
	return err
}

// recoverInterrupted 把进程重启前卡在 running/scraping 的任务恢复为可调度状态。
// 否则 worker 永远 claim 不到，媒体会一直显示“刮削中”。
func (s *Store) recoverInterrupted() error {
	now := s.now().UnixMilli()
	if _, err := s.db.Exec(`UPDATE scrape_jobs SET status='queued', next_run_at=0, updated_at=?
		WHERE status='running'`, now); err != nil {
		return err
	}
	_, err := s.db.Exec(`UPDATE media SET metadata_status='idle', updated_at=?
		WHERE metadata_status='scraping'
		AND id NOT IN (SELECT media_id FROM scrape_jobs WHERE status IN ('queued','running'))`, now)
	return err
}

// EnqueueJob 为媒体排队刮削；已有 job 时不重置（避免每次搜索重复刮）。
func (s *Store) EnqueueJob(mediaID string, now time.Time) error {
	// 只对还没有刮削结果（idle）的媒体排队：失败重试由已存在的 job 负责，
	// 已完成（done）的媒体不能被每次搜索重新刮一遍。
	_, err := s.db.Exec(`INSERT INTO scrape_jobs (media_id, provider, status, attempts, last_error, next_run_at, updated_at)
		SELECT ?, 'auto', 'queued', 0, '', 0, ? FROM media WHERE id=? AND metadata_status='idle'
		ON CONFLICT(media_id, provider) DO NOTHING`, mediaID, now.UnixMilli(), mediaID)
	return err
}

// ResetJob 强制重新排队（人工刷新）。
func (s *Store) ResetJob(mediaID string, now time.Time) error {
	_, err := s.db.Exec(`INSERT INTO scrape_jobs (media_id, provider, status, attempts, last_error, next_run_at, updated_at)
		VALUES (?, 'auto', 'queued', 0, '', 0, ?)
		ON CONFLICT(media_id, provider) DO UPDATE SET status='queued', attempts=0, last_error='', next_run_at=0, updated_at=excluded.updated_at`,
		mediaID, now.UnixMilli())
	return err
}

// ClaimJob 取一条到期任务并标记为 running。
func (s *Store) ClaimJob(now time.Time) (string, int, bool, error) {
	var mediaID string
	var attempts int
	err := s.db.QueryRow(`UPDATE scrape_jobs SET status='running', updated_at=?
		WHERE (media_id, provider) = (
			SELECT media_id, provider FROM scrape_jobs
			WHERE status='queued' AND next_run_at<=?
			ORDER BY next_run_at, updated_at LIMIT 1
		)
		RETURNING media_id, attempts`, now.UnixMilli(), now.UnixMilli()).Scan(&mediaID, &attempts)
	if errors.Is(err, sql.ErrNoRows) {
		return "", 0, false, nil
	}
	return mediaID, attempts, err == nil, err
}

// maxScrapeAttempts 是自动重试上限；超过后任务转为 failed，只能人工刷新。
const maxScrapeAttempts = 3

// FinishJob 结束任务：成功删除；失败按退避重新排队，达到上限后停止自动重试。
func (s *Store) FinishJob(mediaID string, errText string, retryAt time.Time, now time.Time) error {
	if errText == "" {
		_, err := s.db.Exec(`DELETE FROM scrape_jobs WHERE media_id=? AND provider='auto'`, mediaID)
		return err
	}
	// 第 maxScrapeAttempts 次失败后不再自动排队，避免第三方不可达时无限重试。
	_, err := s.db.Exec(`UPDATE scrape_jobs SET
		status = CASE WHEN attempts+1 >= ? THEN 'failed' ELSE 'queued' END,
		attempts = attempts+1, last_error=?, next_run_at=?, updated_at=?
		WHERE media_id=? AND provider='auto'`,
		maxScrapeAttempts, errText, retryAt.UnixMilli(), now.UnixMilli(), mediaID)
	return err
}

// ranked 按分数降序排序候选。
func ranked(cs []Candidate) []Candidate {
	sort.SliceStable(cs, func(i, j int) bool { return cs[i].Score > cs[j].Score })
	return cs
}
