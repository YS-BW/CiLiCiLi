package media

import (
	"crypto/sha1"
	"database/sql"
	"encoding/hex"
	"errors"
	"strings"
	"time"
)

// CoverVariant 是封面尺寸维度：小封面（海报/卡片）与大封面（横版剧照/背景）。
type CoverVariant string

const (
	CoverSmall CoverVariant = "small"
	CoverBig   CoverVariant = "big"
)

// CoverOwner 表示已有封面来源：tmdb / source（采集源或豆瓣等原始图）。
const (
	CoverSrcTMDB   = "tmdb"
	CoverSrcSource = "source"
)

// coverStoreSchema 建统一封面存储表：按 (key, variant) 定位，一张片可同时有大小两种封面。
const coverStoreSchema = `
CREATE TABLE IF NOT EXISTS cover_store (
  key          TEXT NOT NULL,
  variant      TEXT NOT NULL,
  source       TEXT NOT NULL DEFAULT '',
  url          TEXT NOT NULL DEFAULT '',
  local_path   TEXT NOT NULL DEFAULT '',
  content_type TEXT NOT NULL DEFAULT '',
  hash         TEXT NOT NULL DEFAULT '',
  width        INTEGER NOT NULL DEFAULT 0,
  height       INTEGER NOT NULL DEFAULT 0,
  updated_at   INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (key, variant)
);`

// CoverStoreEntry 是统一封面存储的一行。
type CoverStoreEntry struct {
	Key         string
	Variant     CoverVariant
	Source      string
	URL         string
	LocalPath   string
	ContentType string
	Hash        string
	Width       int
	Height      int
	UpdatedAt   int64
}

// coverKey 由媒体身份（标题+年+类型）或源 URL 派生稳定 key。
func coverKey(title, year string, kind Kind, srcURL string) string {
	t := strings.ToLower(strings.TrimSpace(title))
	if t != "" {
		base, _ := NormalizeTitle(title)
		if base == "" {
			base = t
		}
		raw := base + "|" + strings.TrimSpace(year) + "|" + string(kind)
		sum := sha1.Sum([]byte(raw))
		return hex.EncodeToString(sum[:])[:24]
	}
	sum := sha1.Sum([]byte(strings.TrimSpace(srcURL)))
	return hex.EncodeToString(sum[:])[:24]
}

func (s *Store) getCoverEntry(key string, variant CoverVariant) (*CoverStoreEntry, error) {
	row := s.db.QueryRow(`SELECT key, variant, source, url, local_path, content_type, hash, width, height, updated_at
		FROM cover_store WHERE key=? AND variant=?`, key, string(variant))
	e := &CoverStoreEntry{}
	err := row.Scan(&e.Key, &e.Variant, &e.Source, &e.URL, &e.LocalPath, &e.ContentType, &e.Hash, &e.Width, &e.Height, &e.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return e, nil
}

func (s *Store) saveCoverEntry(e CoverStoreEntry, now time.Time) error {
	_, err := s.db.Exec(`INSERT INTO cover_store (key, variant, source, url, local_path, content_type, hash, width, height, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(key, variant) DO UPDATE SET source=excluded.source, url=excluded.url,
		local_path=excluded.local_path, content_type=excluded.content_type, hash=excluded.hash,
		width=excluded.width, height=excluded.height, updated_at=excluded.updated_at`,
		e.Key, string(e.Variant), e.Source, e.URL, e.LocalPath, e.ContentType, e.Hash, e.Width, e.Height, now.UnixMilli())
	return err
}
