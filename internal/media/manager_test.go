package media

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/png"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"cilicili/internal/model"
	"cilicili/internal/upstream"
)

type fakeDoer struct {
	png []byte
}

func (f *fakeDoer) Do(ctx context.Context, rawURL string, opts upstream.Options) (*upstream.Response, error) {
	if strings.Contains(rawURL, ".jpg") || strings.Contains(rawURL, ".png") {
		return &upstream.Response{
			StatusCode: 200,
			Header:     http.Header{"Content-Type": []string{"image/png"}},
			Body:       io.NopCloser(bytes.NewReader(f.png)),
			FinalURL:   rawURL,
		}, nil
	}
	return &upstream.Response{
		StatusCode: 200,
		Header:     http.Header{"Content-Type": []string{"text/html"}},
		Body:       io.NopCloser(strings.NewReader("<html>not an image</html>")),
		FinalURL:   rawURL,
	}, nil
}

func (f *fakeDoer) GetJSON(context.Context, string, upstream.Options) ([]byte, error) {
	return nil, errors.New("not implemented")
}
func (f *fakeDoer) GetText(context.Context, string, upstream.Options) (string, error) {
	return "", errors.New("not implemented")
}

type fakeProvider struct {
	name    string
	enabled bool
	cands   []Candidate
	err     error
	calls   int
}

func (p *fakeProvider) Name() string  { return p.name }
func (p *fakeProvider) Enabled() bool { return p.enabled }
func (p *fakeProvider) Search(context.Context, Request) ([]Candidate, error) {
	p.calls++
	if p.err != nil {
		return nil, p.err
	}
	return p.cands, nil
}

func pngBytes(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 2, 3))
	img.Set(0, 0, color.RGBA{R: 255, A: 255})
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func newTestManager(t *testing.T, providers ...Provider) (*Manager, *fakeDoer) {
	t.Helper()
	store, err := NewStore(t.TempDir() + "/media.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	doer := &fakeDoer{png: pngBytes(t)}
	m, err := NewManager(Config{
		Store:     store,
		Client:    doer,
		Providers: providers,
		CoverDir:  t.TempDir(),
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		CoverBase: "/api/media",
	})
	if err != nil {
		t.Fatal(err)
	}
	return m, doer
}

func items() []model.SearchResultItem {
	return []model.SearchResultItem{
		{SourceKey: "a", SourceName: "源A", VodID: "1", Name: "庆余年", TypeName: "国产剧", Year: "2019", Pic: "https://a.test/1.jpg"},
		{SourceKey: "b", SourceName: "源B", VodID: "2", Name: "庆余年 (2019)", TypeName: "电视剧", Year: "2019", Pic: "https://b.test/2.jpg"},
	}
}

func TestSearchCardsAggregates(t *testing.T) {
	m, _ := newTestManager(t)
	cards, err := m.SearchCards(items(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(cards) != 1 {
		t.Fatalf("cards = %+v", cards)
	}
	if cards[0].SourceCount != 2 || !strings.HasPrefix(cards[0].ID, "media_") {
		t.Fatalf("card = %+v", cards[0])
	}
	if cards[0].Title != "庆余年" || cards[0].Cover == "" {
		t.Fatalf("card = %+v", cards[0])
	}
}

func TestSearchCardsLooseItem(t *testing.T) {
	m, _ := newTestManager(t)
	cards, err := m.SearchCards([]model.SearchResultItem{
		{SourceKey: "a", VodID: "1", Name: "无年份影片", TypeName: "电影"},
	}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(cards) != 1 || cards[0].ID != "src:a:1" || cards[0].SourceCount != 1 {
		t.Fatalf("cards = %+v", cards)
	}
}

func TestRunJobSuggestsAndCachesCover(t *testing.T) {
	p := &fakeProvider{name: "bangumi", enabled: true, cands: []Candidate{{
		ID: "bangumi:1", Provider: "bangumi", ExternalID: "1", Title: "庆余年", Year: "2019",
		Kind: KindTV, Overview: "简介", CoverURL: "https://img.test/t.jpg",
	}}}
	m, _ := newTestManager(t, p)
	ids, err := m.UpsertItems(items(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	id := ids["a\x001"]
	if id == "" {
		t.Fatal("empty media id")
	}
	m.runJob(context.Background(), id, 0)

	media, err := m.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if media.MetadataStatus != MetaDone {
		t.Fatalf("status = %q", media.MetadataStatus)
	}
	if media.MatchStatus != MatchSuggested || len(media.Candidates) != 1 {
		t.Fatalf("match = %q candidates=%+v", media.MatchStatus, media.Candidates)
	}
	if media.Cover.LocalPath == "" || !strings.HasPrefix(media.Cover.URL, "/api/media/") {
		t.Fatalf("cover = %+v", media.Cover)
	}
	if p.calls == 0 {
		t.Fatal("provider not called")
	}
}

func TestMatchConfirmAppliesMetadata(t *testing.T) {
	p := &fakeProvider{name: "bangumi", enabled: true, cands: []Candidate{{
		ID: "bangumi:1", Provider: "bangumi", ExternalID: "1", Title: "庆余年", Year: "2019",
		Kind: KindTV, Overview: "第三方简介",
	}}}
	m, _ := newTestManager(t, p)
	ids, _ := m.UpsertItems(items(), time.Now())
	id := ids["a\x001"]
	m.runJob(context.Background(), id, 0)

	media, err := m.Match(context.Background(), id, "bangumi", "1", "confirm")
	if err != nil {
		t.Fatal(err)
	}
	if media.MatchStatus != MatchConfirmed || media.Provider != "bangumi" || media.ExternalID != "1" {
		t.Fatalf("media = %+v", media)
	}
	if media.Metadata.Overview != "第三方简介" || media.Metadata.Source != "bangumi" {
		t.Fatalf("metadata = %+v", media.Metadata)
	}
}

func TestSetCoverRejectsNonImage(t *testing.T) {
	m, _ := newTestManager(t)
	ids, _ := m.UpsertItems(items(), time.Now())
	id := ids["a\x001"]
	if _, err := m.SetCover(context.Background(), id, "https://a.test/notimage"); !errors.Is(err, ErrCoverType) {
		t.Fatalf("err = %v", err)
	}
}

func TestRunJobProviderFailureRetries(t *testing.T) {
	p := &fakeProvider{name: "bangumi", enabled: true, err: errors.New("boom")}
	m, _ := newTestManager(t, p)
	ids, _ := m.UpsertItems(items(), time.Now())
	id := ids["a\x001"]
	m.runJob(context.Background(), id, 0)

	media, err := m.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if media.MetadataStatus != MetaFailed {
		t.Fatalf("status = %q", media.MetadataStatus)
	}
	// 失败任务应重新排队并带退避时间。
	if _, _, ok, _ := m.store.ClaimJob(time.Now()); ok {
		t.Fatal("retry should not be due immediately")
	}
	if _, _, ok, _ := m.store.ClaimJob(time.Now().Add(6 * time.Minute)); !ok {
		t.Fatal("retry should be due after backoff")
	}
}

func TestProviderCooldown(t *testing.T) {
	p := &fakeProvider{name: "bangumi", enabled: true, err: errors.New("timeout")}
	m, _ := newTestManager(t, p)
	now := time.Now()
	items := []model.SearchResultItem{
		{SourceKey: "a", VodID: "1", Name: "甲", TypeName: "动漫", Year: "2020", Pic: "https://a.test/1.jpg"},
		{SourceKey: "b", VodID: "2", Name: "乙", TypeName: "动漫", Year: "2021", Pic: "https://b.test/2.jpg"},
	}
	ids, err := m.UpsertItems(items, now)
	if err != nil {
		t.Fatal(err)
	}
	first := ids["a\x001"]
	second := ids["b\x002"]
	m.runJob(context.Background(), first, 0)
	if p.calls != 1 {
		t.Fatalf("provider calls = %d", p.calls)
	}
	// 冷却期内第二个任务不再调用 provider，也不会被标成 failed。
	m.runJob(context.Background(), second, 0)
	if p.calls != 1 {
		t.Fatalf("provider should be cooled down, calls = %d", p.calls)
	}
	media, err := m.Get(context.Background(), second)
	if err != nil {
		t.Fatal(err)
	}
	if media.MetadataStatus != MetaDone {
		t.Fatalf("second media status = %q", media.MetadataStatus)
	}
}
