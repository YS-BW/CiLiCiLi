package httpx

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
)

// MaxJSONBody 是普通 JSON 请求体的大小上限。
const MaxJSONBody = 1 << 20 // 1 MiB

// ErrorResponse 是统一的错误响应结构。
type ErrorResponse struct {
	Error string `json:"error"`
}

// WriteJSON 写出 JSON 响应。
func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	if v == nil {
		return
	}
	_ = json.NewEncoder(w).Encode(v)
}

// WriteError 写出统一格式的错误响应。
func WriteError(w http.ResponseWriter, status int, msg string) {
	WriteJSON(w, status, ErrorResponse{Error: msg})
}

// ReadJSON 读取并解析请求体（限制大小）。
func ReadJSON(r *http.Request, dst any) error {
	if r.Body == nil {
		return errors.New("请求体为空")
	}
	defer r.Body.Close()
	dec := json.NewDecoder(io.LimitReader(r.Body, MaxJSONBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return fmt.Errorf("请求格式错误: %w", err)
	}
	return nil
}

// ClientIP 取真实客户端 IP。优先使用 nginx 注入的 X-Real-IP，
// 不直接信任可伪造的 X-Forwarded-For。
func ClientIP(r *http.Request) string {
	if ip := strings.TrimSpace(r.Header.Get("X-Real-IP")); ip != "" {
		return ip
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// NDJSONWriter 逐行写 NDJSON 并 flush。
type NDJSONWriter struct {
	w       http.ResponseWriter
	flusher http.Flusher
	enc     *json.Encoder
}

// NewNDJSONWriter 准备 NDJSON 响应头。调用方应先设置状态码（默认 200）。
func NewNDJSONWriter(w http.ResponseWriter) *NDJSONWriter {
	w.Header().Set("Content-Type", "application/x-ndjson; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	f, _ := w.(http.Flusher)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	return &NDJSONWriter{w: w, flusher: f, enc: enc}
}

// Write 写一个事件并 flush。
func (n *NDJSONWriter) Write(v any) error {
	if err := n.enc.Encode(v); err != nil {
		return err
	}
	if n.flusher != nil {
		n.flusher.Flush()
	}
	return nil
}
