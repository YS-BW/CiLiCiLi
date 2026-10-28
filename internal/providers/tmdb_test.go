package providers

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"cilicili/internal/media"
	"cilicili/internal/upstream"
)

type fakeDoer struct {
	lastURL  string
	lastOpts upstream.Options
	body     string
	err      error
	calls    int
}

func (f *fakeDoer) Do(_ context.Context, rawURL string, opts upstream.Options) (*upstream.Response, error) {
	f.calls++
	f.lastURL = rawURL
	f.lastOpts = opts
	if f.err != nil {
		return nil, f.err
	}
	return &upstream.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(f.body)),
		FinalURL:   rawURL,
	}, nil
}

func (f *fakeDoer) GetJSON(ctx context.Context, rawURL string, opts upstream.Options) ([]byte, error) {
	resp, err := f.Do(ctx, rawURL, opts)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}

func (f *fakeDoer) GetText(ctx context.Context, rawURL string, opts upstream.Options) (string, error) {
	b, err := f.GetJSON(ctx, rawURL, opts)
	return string(b), err
}

func TestTMDBDisabledWithoutCredentials(t *testing.T) {
	p := NewTMDB(&fakeDoer{}, "", "", "")
	if p.Enabled() {
		t.Fatal("空凭据不应启用")
	}
	if cs, err := p.Search(context.Background(), media.Request{Kind: media.KindMovie, Title: "x"}); err != nil || cs != nil {
		t.Fatalf("cs=%v err=%v", cs, err)
	}
}

func TestTMDBMovieSearchWithAPIKey(t *testing.T) {
	doer := &fakeDoer{body: `{"results":[
		{"id":1,"title":"庆余年","original_title":"Joy of Life","overview":"简介","poster_path":"/a.jpg","backdrop_path":"/bd.jpg","release_date":"2019-11-26"},
		{"id":2,"title":"别的","poster_path":"","release_date":""}
	]}`}
	p := NewTMDB(doer, "KEY", "", "")
	cs, err := p.Search(context.Background(), media.Request{Kind: media.KindMovie, Title: "庆余年", Year: "2019"})
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 2 {
		t.Fatalf("candidates = %+v", cs)
	}
	if !strings.Contains(doer.lastURL, "/3/search/movie") ||
		!strings.Contains(doer.lastURL, "api_key=KEY") ||
		!strings.Contains(doer.lastURL, "language=zh-CN") ||
		strings.Contains(doer.lastURL, "year=") {
		t.Fatalf("url = %s", doer.lastURL)
	}
	if got := doer.lastOpts.Headers.Get("Authorization"); got != "" {
		t.Fatalf("api_key 模式不应带 Authorization，got %q", got)
	}
	c := cs[0]
	if c.Provider != "tmdb" || c.ExternalID != "1" || c.Year != "2019" || c.Kind != media.KindMovie {
		t.Fatalf("candidate = %+v", c)
	}
	if c.CoverURL != "https://image.tmdb.org/t/p/w500/a.jpg" {
		t.Fatalf("cover = %q", c.CoverURL)
	}
	if c.BackdropURL != "https://image.tmdb.org/t/p/w1280/bd.jpg" {
		t.Fatalf("backdrop = %q", c.BackdropURL)
	}
	if cs[1].CoverURL != "" || cs[1].Year != "" || cs[1].BackdropURL != "" {
		t.Fatalf("空字段不应编造: %+v", cs[1])
	}
}

func TestTMDBTVSearchWithBearerToken(t *testing.T) {
	doer := &fakeDoer{body: `{"results":[{"id":9,"name":"庆余年","original_name":"Joy of Life","first_air_date":"2019-11-26"}]}`}
	p := NewTMDB(doer, "KEY", "TOKEN", "")
	cs, err := p.Search(context.Background(), media.Request{Kind: media.KindTV, Title: "庆余年"})
	if err != nil || len(cs) != 1 {
		t.Fatalf("cs=%v err=%v", cs, err)
	}
	if !strings.Contains(doer.lastURL, "/3/search/tv") || strings.Contains(doer.lastURL, "api_key=") {
		t.Fatalf("url = %s", doer.lastURL)
	}
	if got := doer.lastOpts.Headers.Get("Authorization"); got != "Bearer TOKEN" {
		t.Fatalf("Authorization = %q", got)
	}
	if cs[0].Title != "庆余年" || cs[0].Year != "2019" || cs[0].Kind != media.KindTV {
		t.Fatalf("candidate = %+v", cs[0])
	}
}

func TestTMDBAnimeUsesTVEndpoint(t *testing.T) {
	doer := &fakeDoer{body: `{"results":[]}`}
	p := NewTMDB(doer, "KEY", "", "")
	if _, err := p.Search(context.Background(), media.Request{Kind: media.KindAnime, Title: "番"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(doer.lastURL, "/3/search/tv") {
		t.Fatalf("url = %s", doer.lastURL)
	}
}

func TestTMDBUnknownKindSkipsRequest(t *testing.T) {
	doer := &fakeDoer{}
	p := NewTMDB(doer, "KEY", "", "")
	cs, err := p.Search(context.Background(), media.Request{Kind: media.KindOther, Title: "x"})
	if err != nil || cs != nil || doer.calls != 0 {
		t.Fatalf("cs=%v err=%v calls=%d", cs, err, doer.calls)
	}
}

func TestTMDBUpstreamErrorIsWrapped(t *testing.T) {
	doer := &fakeDoer{err: errors.New("boom")}
	p := NewTMDB(doer, "KEY", "", "")
	if _, err := p.Search(context.Background(), media.Request{Kind: media.KindMovie, Title: "x"}); err == nil ||
		!strings.Contains(err.Error(), "tmdb request") {
		t.Fatalf("err = %v", err)
	}
}

func TestTMDBDefaultBaseIsAPITMDB(t *testing.T) {
	p := NewTMDB(&fakeDoer{}, "KEY", "", "")
	if p.base != "https://api.tmdb.org" {
		t.Fatalf("base = %q", p.base)
	}
}
