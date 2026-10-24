// Package sources 持久化点播源配置与订阅。
//
// 设计：
//   - SQLite 单文件（DATA_DIR/sources.db），启动时若为空则从 SOURCES_FILE 导入一次；
//   - sources 表保存启用状态与排序，Registry 只装载 enabled 的源；
//   - subscriptions 表保存订阅地址与最近一次拉取结果，供管理页展示。
package sources

import (
	"crypto/sha1"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"cilicili/internal/model"
)

// ErrNotFound 表示源/订阅不存在。
var ErrNotFound = errors.New("记录不存在")

// ErrConflict 表示 key 或 URL 已存在。
var ErrConflict = errors.New("记录已存在")

// ErrInvalidURL 表示源地址不合法。
var ErrInvalidURL = errors.New("无效的源地址")

const schemaSQL = `
CREATE TABLE IF NOT EXISTS sources (
  key        TEXT PRIMARY KEY,
  name       TEXT NOT NULL DEFAULT '',
  url        TEXT NOT NULL,
  detail     TEXT NOT NULL DEFAULT '',
  is_adult   INTEGER NOT NULL DEFAULT 0,
  enabled    INTEGER NOT NULL DEFAULT 1,
  sort_order INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL DEFAULT 0,
  updated_at INTEGER NOT NULL DEFAULT 0
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_sources_url ON sources(url);

CREATE TABLE IF NOT EXISTS subscriptions (
  id            INTEGER PRIMARY KEY AUTOINCREMENT,
  name          TEXT NOT NULL DEFAULT '',
  url           TEXT NOT NULL UNIQUE,
  enabled       INTEGER NOT NULL DEFAULT 1,
  last_fetch_at INTEGER NOT NULL DEFAULT 0,
  last_status   TEXT NOT NULL DEFAULT '',
  last_error    TEXT NOT NULL DEFAULT '',
  last_count    INTEGER NOT NULL DEFAULT 0,
  updated_at    INTEGER NOT NULL DEFAULT 0
);
`

// Source 是持久化的点播源（含启用状态与排序）。
type Source struct {
	model.SourceConfig
	Enabled   bool  `json:"enabled"`
	SortOrder int   `json:"sortOrder"`
	CreatedAt int64 `json:"createdAt,omitempty"`
	UpdatedAt int64 `json:"updatedAt,omitempty"`
}

// Subscription 是远程源订阅。
type Subscription struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	URL         string `json:"url"`
	Enabled     bool   `json:"enabled"`
	LastFetchAt int64  `json:"lastFetchAt,omitempty"`
	LastStatus  string `json:"lastStatus,omitempty"`
	LastError   string `json:"lastError,omitempty"`
	LastCount   int    `json:"lastCount"`
	UpdatedAt   int64  `json:"updatedAt,omitempty"`
}

// SourcePatch 是更新源时的可选字段。
type SourcePatch struct {
	Name    *string
	URL     *string
	Detail  *string
	IsAdult *bool
	Enabled *bool
}

// Store 是 sources.db 的访问封装。
type Store struct {
	db  *sql.DB
	now func() time.Time
}

// New 打开/创建 sources.db。
func New(path string) (*Store, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("源配置库路径不能为空")
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return nil, fmt.Errorf("创建源配置目录: %w", err)
		}
	}
	dsn := path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("打开源配置库: %w", err)
	}
	db.SetMaxOpenConns(2)
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("连接源配置库: %w", err)
	}
	if _, err := db.Exec(schemaSQL); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("初始化源配置库: %w", err)
	}
	return &Store{db: db, now: time.Now}, nil
}

// Close 关闭数据库。
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

// CountSources 返回源数量。
func (s *Store) CountSources() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM sources`).Scan(&n)
	return n, err
}

// Seed 在库为空时导入默认源（来自 SOURCES_FILE），只执行一次。
func (s *Store) Seed(defaults []model.SourceConfig) error {
	n, err := s.CountSources()
	if err != nil {
		return err
	}
	if n > 0 || len(defaults) == 0 {
		return nil
	}
	now := s.now().UnixMilli()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for i, src := range defaults {
		key := strings.TrimSpace(src.Key)
		if key == "" {
			key = NewKey(src.URL)
		}
		if _, err := tx.Exec(`INSERT INTO sources (key,name,url,detail,is_adult,enabled,sort_order,created_at,updated_at)
			VALUES (?,?,?,?,?,1,?,?,?)
			ON CONFLICT(key) DO NOTHING`,
			key, src.Name, src.URL, src.Detail, boolInt(src.IsAdult), i, now, now); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// List 返回全部源（含禁用），按排序返回。
func (s *Store) List() ([]Source, error) {
	rows, err := s.db.Query(`SELECT key,name,url,detail,is_adult,enabled,sort_order,created_at,updated_at
		FROM sources ORDER BY sort_order, key`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Source, 0, 16)
	for rows.Next() {
		var (
			src     Source
			isAdult int
			enabled int
		)
		if err := rows.Scan(&src.Key, &src.Name, &src.URL, &src.Detail, &isAdult, &enabled,
			&src.SortOrder, &src.CreatedAt, &src.UpdatedAt); err != nil {
			return nil, err
		}
		src.IsAdult = isAdult != 0
		src.Enabled = enabled != 0
		out = append(out, src)
	}
	return out, rows.Err()
}

// ListEnabled 返回启用中的源（给 Registry 用）。
func (s *Store) ListEnabled() ([]model.SourceConfig, error) {
	all, err := s.List()
	if err != nil {
		return nil, err
	}
	out := make([]model.SourceConfig, 0, len(all))
	for _, src := range all {
		if src.Enabled {
			out = append(out, src.SourceConfig)
		}
	}
	return out, nil
}

// Get 按 key 查源。
func (s *Store) Get(key string) (*Source, error) {
	row := s.db.QueryRow(`SELECT key,name,url,detail,is_adult,enabled,sort_order,created_at,updated_at
		FROM sources WHERE key=?`, key)
	var (
		src     Source
		isAdult int
		enabled int
	)
	err := row.Scan(&src.Key, &src.Name, &src.URL, &src.Detail, &isAdult, &enabled,
		&src.SortOrder, &src.CreatedAt, &src.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	src.IsAdult = isAdult != 0
	src.Enabled = enabled != 0
	return &src, nil
}

// Create 新增源；key 为空时按 URL 生成，默认启用。
func (s *Store) Create(src Source) (*Source, error) {
	return s.CreateWithEnabled(src, true)
}

// CreateWithEnabled 新增源并指定启用状态。
func (s *Store) CreateWithEnabled(src Source, enabled bool) (*Source, error) {
	normURL, err := NormalizeURL(src.URL)
	if err != nil {
		return nil, err
	}
	key := strings.TrimSpace(src.Key)
	if key == "" {
		key = NewKey(normURL)
	}
	now := s.now().UnixMilli()
	var nextOrder int
	_ = s.db.QueryRow(`SELECT COALESCE(MAX(sort_order)+1,0) FROM sources`).Scan(&nextOrder)
	_, err = s.db.Exec(`INSERT INTO sources (key,name,url,detail,is_adult,enabled,sort_order,created_at,updated_at)
		VALUES (?,?,?,?,?,?,?,?,?)`,
		key, strings.TrimSpace(src.Name), normURL, strings.TrimSpace(src.Detail),
		boolInt(src.IsAdult), boolInt(enabled), nextOrder, now, now)
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "unique") {
			return nil, ErrConflict
		}
		return nil, err
	}
	return s.Get(key)
}

// Update 更新源的字段（只改传入的字段）。
func (s *Store) Update(key string, patch SourcePatch) (*Source, error) {
	cur, err := s.Get(key)
	if err != nil {
		return nil, err
	}
	if patch.Name != nil {
		cur.Name = strings.TrimSpace(*patch.Name)
	}
	if patch.Detail != nil {
		cur.Detail = strings.TrimSpace(*patch.Detail)
	}
	if patch.IsAdult != nil {
		cur.IsAdult = *patch.IsAdult
	}
	if patch.Enabled != nil {
		cur.Enabled = *patch.Enabled
	}
	if patch.URL != nil {
		norm, err := NormalizeURL(*patch.URL)
		if err != nil {
			return nil, err
		}
		cur.URL = norm
	}
	_, err = s.db.Exec(`UPDATE sources SET name=?,url=?,detail=?,is_adult=?,enabled=?,updated_at=? WHERE key=?`,
		cur.Name, cur.URL, cur.Detail, boolInt(cur.IsAdult), boolInt(cur.Enabled), s.now().UnixMilli(), key)
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "unique") {
			return nil, ErrConflict
		}
		return nil, err
	}
	return s.Get(key)
}

// Delete 删除源。
func (s *Store) Delete(key string) error {
	res, err := s.db.Exec(`DELETE FROM sources WHERE key=?`, key)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// Reorder 按给定 key 顺序重排；未列出的源保持原相对顺序排在后面。
func (s *Store) Reorder(keys []string) error {
	all, err := s.List()
	if err != nil {
		return err
	}
	want := make(map[string]bool, len(keys))
	for _, k := range keys {
		want[k] = true
	}
	ordered := make([]string, 0, len(all))
	for _, k := range keys {
		for _, src := range all {
			if src.Key == k {
				ordered = append(ordered, k)
				break
			}
		}
	}
	for _, src := range all {
		if !want[src.Key] {
			ordered = append(ordered, src.Key)
		}
	}
	now := s.now().UnixMilli()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for i, k := range ordered {
		if _, err := tx.Exec(`UPDATE sources SET sort_order=?, updated_at=? WHERE key=?`, i, now, k); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ---- 订阅 ----

// ListSubscriptions 返回全部订阅。
func (s *Store) ListSubscriptions() ([]Subscription, error) {
	rows, err := s.db.Query(`SELECT id,name,url,enabled,last_fetch_at,last_status,last_error,last_count,updated_at
		FROM subscriptions ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Subscription, 0, 8)
	for rows.Next() {
		var (
			sub     Subscription
			enabled int
		)
		if err := rows.Scan(&sub.ID, &sub.Name, &sub.URL, &enabled, &sub.LastFetchAt,
			&sub.LastStatus, &sub.LastError, &sub.LastCount, &sub.UpdatedAt); err != nil {
			return nil, err
		}
		sub.Enabled = enabled != 0
		out = append(out, sub)
	}
	return out, rows.Err()
}

// GetSubscription 按 id 查订阅。
func (s *Store) GetSubscription(id int64) (*Subscription, error) {
	row := s.db.QueryRow(`SELECT id,name,url,enabled,last_fetch_at,last_status,last_error,last_count,updated_at
		FROM subscriptions WHERE id=?`, id)
	var (
		sub     Subscription
		enabled int
	)
	err := row.Scan(&sub.ID, &sub.Name, &sub.URL, &enabled, &sub.LastFetchAt,
		&sub.LastStatus, &sub.LastError, &sub.LastCount, &sub.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	sub.Enabled = enabled != 0
	return &sub, nil
}

// CreateSubscription 新增订阅。
func (s *Store) CreateSubscription(name, rawURL string) (*Subscription, error) {
	norm, err := NormalizeURL(rawURL)
	if err != nil {
		return nil, err
	}
	now := s.now().UnixMilli()
	res, err := s.db.Exec(`INSERT INTO subscriptions (name,url,enabled,updated_at) VALUES (?,?,1,?)`,
		strings.TrimSpace(name), norm, now)
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "unique") {
			return nil, ErrConflict
		}
		return nil, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}
	return s.GetSubscription(id)
}

// UpdateSubscription 更新订阅名称/地址/启用状态。
func (s *Store) UpdateSubscription(id int64, name, rawURL *string, enabled *bool) (*Subscription, error) {
	cur, err := s.GetSubscription(id)
	if err != nil {
		return nil, err
	}
	if name != nil {
		cur.Name = strings.TrimSpace(*name)
	}
	if rawURL != nil {
		norm, err := NormalizeURL(*rawURL)
		if err != nil {
			return nil, err
		}
		cur.URL = norm
	}
	if enabled != nil {
		cur.Enabled = *enabled
	}
	_, err = s.db.Exec(`UPDATE subscriptions SET name=?,url=?,enabled=?,updated_at=? WHERE id=?`,
		cur.Name, cur.URL, boolInt(cur.Enabled), s.now().UnixMilli(), id)
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "unique") {
			return nil, ErrConflict
		}
		return nil, err
	}
	return s.GetSubscription(id)
}

// DeleteSubscription 删除订阅。
func (s *Store) DeleteSubscription(id int64) error {
	res, err := s.db.Exec(`DELETE FROM subscriptions WHERE id=?`, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// SetSubscriptionResult 记录一次拉取结果。
func (s *Store) SetSubscriptionResult(id int64, status, errText string, count int) error {
	_, err := s.db.Exec(`UPDATE subscriptions SET last_fetch_at=?, last_status=?, last_error=?, last_count=?, updated_at=? WHERE id=?`,
		s.now().UnixMilli(), status, errText, count, s.now().UnixMilli(), id)
	return err
}

// NewKey 按 URL 生成稳定 key（同 subscribe 的 sub_ 风格，这里用 src_ 区分手工源）。
func NewKey(rawURL string) string {
	sum := sha1.Sum([]byte(strings.TrimSpace(rawURL)))
	return "src_" + hex.EncodeToString(sum[:])[:8]
}

// NormalizeURL 校验并规范化源地址。
func NormalizeURL(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" || len(s) > 2048 {
		return "", ErrInvalidURL
	}
	u, err := url.Parse(s)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", ErrInvalidURL
	}
	return strings.TrimRight(s, "/"), nil
}

func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}
