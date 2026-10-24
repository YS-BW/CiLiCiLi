package media

import "context"

// Request 是一次元数据搜索的输入。
type Request struct {
	Kind  Kind
	Title string
	Year  string
	Hint  string
}

// Provider 是元数据源接口。实现放在 internal/providers，避免循环依赖：
// providers 依赖 media.Candidate，media 只依赖这个接口。
type Provider interface {
	Name() string
	Enabled() bool
	Search(ctx context.Context, req Request) ([]Candidate, error)
}
