package media

import (
	"path/filepath"
	"testing"
	"time"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := NewStore(filepath.Join(t.TempDir(), "media.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestUpsertAggregatesSameMovie(t *testing.T) {
	s := newTestStore(t)
	now := time.Now()
	refs := []SourceRef{
		{SourceKey: "a", SourceName: "源A", VodID: "1", Name: "庆余年", TypeName: "国产剧", Year: "2019", Pic: "https://a.test/1.jpg"},
		{SourceKey: "b", SourceName: "源B", VodID: "2", Name: "庆余年 (2019)", TypeName: "电视剧", Year: "2019", Pic: "https://b.test/2.jpg"},
	}
	ids, err := s.UpsertSourceItems(refs, now)
	if err != nil {
		t.Fatal(err)
	}
	idA := ids["a\x001"]
	idB := ids["b\x002"]
	if idA == "" || idA != idB {
		t.Fatalf("ids = %q, %q", idA, idB)
	}
	m, err := s.GetMedia(idA)
	if err != nil {
		t.Fatal(err)
	}
	if m == nil || m.SourceCount != 2 {
		t.Fatalf("media = %+v", m)
	}
	if m.Title != "庆余年" || m.Year != "2019" {
		t.Fatalf("title/year = %q/%q", m.Title, m.Year)
	}
	if len(m.Sources) != 2 || m.Sources[0].SourceKey != "a" {
		t.Fatalf("sources = %+v", m.Sources)
	}
}

func TestUpsertMissingYearNotAggregated(t *testing.T) {
	s := newTestStore(t)
	refs := []SourceRef{
		{SourceKey: "a", VodID: "1", Name: "未知影片", TypeName: "电影", Year: ""},
		{SourceKey: "b", VodID: "2", Name: "未知影片", TypeName: "电影", Year: ""},
	}
	ids, err := s.UpsertSourceItems(refs, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if ids["a\x001"] != "" || ids["b\x002"] != "" {
		t.Fatalf("ids = %+v", ids)
	}
	stats, _ := s.Stats()
	if stats["media"] != 0 {
		t.Fatalf("media count = %d", stats["media"])
	}
}

func TestFindBySourceAndCandidates(t *testing.T) {
	s := newTestStore(t)
	now := time.Now()
	ids, err := s.UpsertSourceItems([]SourceRef{
		{SourceKey: "a", VodID: "1", Name: "庆余年", TypeName: "国产剧", Year: "2019"},
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	id := ids["a\x001"]
	if id == "" {
		t.Fatal("empty id")
	}
	got, err := s.FindMediaBySource("a", "1")
	if err != nil || got != id {
		t.Fatalf("find = %q, %v", got, err)
	}
	if err := s.SaveCandidates(id, []Candidate{
		{ID: "bangumi:1", Provider: "bangumi", ExternalID: "1", Title: "庆余年", Score: 0.9},
		{ID: "bangumi:2", Provider: "bangumi", ExternalID: "2", Title: "庆余年", Score: 0.5},
	}, now); err != nil {
		t.Fatal(err)
	}
	cs, err := s.Candidates(id)
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 2 || cs[0].ExternalID != "1" {
		t.Fatalf("candidates = %+v", cs)
	}
}

func TestJobLifecycle(t *testing.T) {
	s := newTestStore(t)
	now := time.Now()
	ids, _ := s.UpsertSourceItems([]SourceRef{{SourceKey: "a", VodID: "1", Name: "X", TypeName: "电影", Year: "2020"}}, now)
	id := ids["a\x001"]
	if err := s.EnqueueJob(id, now); err != nil {
		t.Fatal(err)
	}
	// 重复入队不应重置运行中的任务。
	claimed, attempts, ok, err := s.ClaimJob(now)
	if err != nil || !ok || claimed != id || attempts != 0 {
		t.Fatalf("claim = %q,%d,%v,%v", claimed, attempts, ok, err)
	}
	if _, _, ok, _ := s.ClaimJob(now); ok {
		t.Fatal("processing job should not be claimed twice")
	}
	// 失败后按退避时间重新排队。
	if err := s.FinishJob(id, "boom", now.Add(5*time.Minute), now); err != nil {
		t.Fatal(err)
	}
	if _, _, ok, _ := s.ClaimJob(now); ok {
		t.Fatal("job should not be due yet")
	}
	if claimed, attempts, ok, _ := s.ClaimJob(now.Add(6 * time.Minute)); !ok || claimed != id || attempts != 1 {
		t.Fatalf("retry claim = %q,%d,%v", claimed, attempts, ok)
	}
	// 成功结束会删除任务。
	if err := s.FinishJob(id, "", time.Time{}, now); err != nil {
		t.Fatal(err)
	}
	if _, _, ok, _ := s.ClaimJob(now.Add(24 * time.Hour)); ok {
		t.Fatal("job should be deleted")
	}
}

func TestCoverStore(t *testing.T) {
	s := newTestStore(t)
	now := time.Now()
	ids, _ := s.UpsertSourceItems([]SourceRef{{SourceKey: "a", VodID: "1", Name: "X", TypeName: "电影", Year: "2020"}}, now)
	id := ids["a\x001"]
	ok := true
	if err := s.SaveCover(id, CoverInfo{
		URL: "https://a.test/1.jpg", LocalPath: "/tmp/x.jpg", Hash: "abc", Width: 100, Height: 150,
		Source: "a", Reachable: &ok, UpdatedAt: now.UnixMilli(),
	}, now); err != nil {
		t.Fatal(err)
	}
	ci, err := s.getCover(id)
	if err != nil || ci == nil {
		t.Fatalf("cover = %+v, %v", ci, err)
	}
	if ci.Hash != "abc" || ci.Width != 100 || ci.Reachable == nil || !*ci.Reachable {
		t.Fatalf("cover = %+v", ci)
	}
}

func TestEnqueueSkipsDoneMedia(t *testing.T) {
	s := newTestStore(t)
	now := time.Now()
	ids, _ := s.UpsertSourceItems([]SourceRef{{SourceKey: "a", VodID: "1", Name: "X", TypeName: "电影", Year: "2020"}}, now)
	id := ids["a\x001"]
	if err := s.SetMetadataStatus(id, MetaDone); err != nil {
		t.Fatal(err)
	}
	if err := s.EnqueueJob(id, now); err != nil {
		t.Fatal(err)
	}
	if _, _, ok, _ := s.ClaimJob(now); ok {
		t.Fatal("done media should not be enqueued")
	}
	// Refresh 可以强制重新排队。
	if err := s.SetMetadataStatus(id, MetaQueued); err != nil {
		t.Fatal(err)
	}
	if err := s.ResetJob(id, now); err != nil {
		t.Fatal(err)
	}
	if claimed, _, ok, _ := s.ClaimJob(now); !ok || claimed != id {
		t.Fatalf("reset job claim = %q,%v", claimed, ok)
	}
}

func TestRecoverInterruptedJobs(t *testing.T) {
	path := t.TempDir() + "/media.db"
	s, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	ids, _ := s.UpsertSourceItems([]SourceRef{{SourceKey: "a", VodID: "1", Name: "X", TypeName: "电影", Year: "2020"}}, now)
	id := ids["a\x001"]
	_ = s.EnqueueJob(id, now)
	if _, _, ok, _ := s.ClaimJob(now); !ok {
		t.Fatal("claim failed")
	}
	_ = s.SetMetadataStatus(id, MetaScraping)
	// 模拟进程崩溃：不 Finish 直接关闭。
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s2, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	claimed, _, ok, err := s2.ClaimJob(now.Add(time.Second))
	if err != nil || !ok || claimed != id {
		t.Fatalf("recovered claim = %q,%v,%v", claimed, ok, err)
	}
}

func TestFinishJobStopsAfterMaxAttempts(t *testing.T) {
	s := newTestStore(t)
	now := time.Now()
	ids, _ := s.UpsertSourceItems([]SourceRef{{SourceKey: "a", VodID: "1", Name: "X", TypeName: "电影", Year: "2020"}}, now)
	id := ids["a\x001"]
	_ = s.EnqueueJob(id, now)
	for i := 0; i < maxScrapeAttempts; i++ {
		if _, _, ok, _ := s.ClaimJob(now.Add(time.Duration(i) * time.Hour)); !ok {
			t.Fatalf("claim %d failed", i)
		}
		if err := s.FinishJob(id, "boom", now.Add(time.Hour), now.Add(time.Duration(i)*time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, ok, _ := s.ClaimJob(now.Add(24 * time.Hour)); ok {
		t.Fatal("job should stop retrying after max attempts")
	}
}
