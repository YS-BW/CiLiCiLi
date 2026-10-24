package media

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	_ "golang.org/x/image/webp"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"cilicili/internal/upstream"
)

const (
	// maxCoverBytes 是单张封面的硬上限。
	maxCoverBytes = 8 << 20
	// coverFetchTimeout 是下载封面的总超时。
	coverFetchTimeout = 15 * time.Second
)

// ErrCoverTooLarge / ErrCoverType 便于 API 层返回 400。
var (
	ErrCoverTooLarge = errors.New("封面文件过大")
	ErrCoverType     = errors.New("封面不是受支持的图片类型")
)

// coverExt 根据 content-type 选扩展名。
func coverExt(contentType string) string {
	ct := strings.ToLower(strings.TrimSpace(contentType))
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = ct[:i]
	}
	switch ct {
	case "image/jpeg", "image/jpg":
		return ".jpg"
	case "image/png":
		return ".png"
	case "image/webp":
		return ".webp"
	case "image/gif":
		return ".gif"
	default:
		return ""
	}
}

// downloadCover 下载并校验图片，返回落盘信息。图片按 mediaID 命名，覆盖旧文件。
func (m *Manager) downloadCover(ctx context.Context, mediaID, rawURL, source string) (CoverInfo, error) {
	info := CoverInfo{URL: rawURL, Source: source}
	if strings.TrimSpace(rawURL) == "" {
		return info, errors.New("封面地址为空")
	}
	res, err := m.client.Do(ctx, rawURL, upstream.Options{
		Timeout:       coverFetchTimeout,
		MaxBytes:      maxCoverBytes,
		Retries:       1,
		SafeRedirects: true,
		Headers:       http.Header{"Accept": []string{"image/*"}},
	})
	if err != nil {
		info.Reachable = boolPtr(false)
		return info, err
	}
	defer res.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		info.Reachable = boolPtr(false)
		return info, fmt.Errorf("封面返回 HTTP %d", res.StatusCode)
	}
	ext := coverExt(res.Header.Get("Content-Type"))
	if ext == "" {
		info.Reachable = boolPtr(false)
		return info, ErrCoverType
	}
	if res.Header.Get("Content-Length") != "" && res.Header.Get("Content-Length") != "0" {
		if n, err := parsePositiveInt(res.Header.Get("Content-Length")); err == nil && n > maxCoverBytes {
			info.Reachable = boolPtr(false)
			return info, ErrCoverTooLarge
		}
	}
	data, err := io.ReadAll(io.LimitReader(res.Body, maxCoverBytes+1))
	if err != nil {
		info.Reachable = boolPtr(false)
		return info, err
	}
	if len(data) > maxCoverBytes {
		info.Reachable = boolPtr(false)
		return info, ErrCoverTooLarge
	}
	// 解码配置：确认确实是图片，并拿到宽高。
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		info.Reachable = boolPtr(false)
		return info, ErrCoverType
	}
	sum := sha256.Sum256(data)
	info.Hash = hex.EncodeToString(sum[:])[:32]
	info.Width = cfg.Width
	info.Height = cfg.Height
	info.Reachable = boolPtr(true)
	info.UpdatedAt = time.Now().UnixMilli()

	if err := os.MkdirAll(m.coverDir, 0o750); err != nil {
		return info, err
	}
	path := filepath.Join(m.coverDir, mediaID+ext)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o640); err != nil {
		return info, err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return info, err
	}
	info.LocalPath = path
	return info, nil
}

func parsePositiveInt(s string) (int64, error) {
	var n int64
	if s == "" {
		return 0, errors.New("empty")
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, errors.New("not a number")
		}
		n = n*10 + int64(r-'0')
		if n > 1<<40 {
			return 0, errors.New("too large")
		}
	}
	return n, nil
}

// CoverPath 返回媒体封面本地路径；文件不存在返回空。
func (s *Store) CoverPath(mediaID string) (string, error) {
	ci, err := s.getCover(mediaID)
	if err != nil || ci == nil {
		return "", err
	}
	if ci.LocalPath == "" {
		return "", nil
	}
	if _, err := os.Stat(ci.LocalPath); err != nil {
		return "", nil
	}
	return ci.LocalPath, nil
}

func boolPtr(b bool) *bool { return &b }
