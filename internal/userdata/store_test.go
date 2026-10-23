package userdata

import (
	"path/filepath"
	"testing"
	"time"
)

func newStore(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "userdata.db")
	s, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, path
}

func TestProgressPutAndHistory(t *testing.T) {
	s, _ := newStore(t)
	base := time.Unix(1000, 0)
	p1 := Progress{
		SourceKey: "s1", SourceName: "源1", VodID: "101", Title: "影片A", Cover: "https://img/a.jpg",
		LineID: "l1", LineName: "线路1", EpisodeID: "l1e3", EpisodeName: "第03集", EpisodeIndex: 3,
		Position: 120, Duration: 2700,
	}
	saved, err := s.PutProgress(p1, base)
	if err != nil {
		t.Fatal(err)
	}
	if saved.ID != "s1:101" {
		t.Fatalf("id = %q", saved.ID)
	}
	if saved.UpdatedAt != base.UnixMilli() {
		t.Fatalf("updatedAt = %d", saved.UpdatedAt)
	}

	// 同一影片再上报会覆盖，不会产生第二条历史。
	_, err = s.PutProgress(Progress{
		SourceKey: "s1", VodID: "101", EpisodeID: "l1e4", Position: 300, Duration: 2700,
	}, base.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	// 另一部影片。
	_, err = s.PutProgress(Progress{
		SourceKey: "s2", VodID: "202", EpisodeID: "l1e1", Position: 30, Duration: 1200,
	}, base.Add(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	// 看完的条目。
	_, err = s.PutProgress(Progress{
		SourceKey: "s3", VodID: "303", EpisodeID: "l1e1", Position: 1200, Duration: 1200, Completed: true,
	}, base.Add(3*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	// 0 进度不应出现在继续观看。
	_, err = s.PutProgress(Progress{SourceKey: "s4", VodID: "404", EpisodeID: "l1e1", Position: 0}, base.Add(4*time.Minute))
	if err != nil {
		t.Fatal(err)
	}

	all, err := s.History(false, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 4 {
		t.Fatalf("history = %d: %+v", len(all), all)
	}
	if all[0].SourceKey != "s4" {
		t.Fatalf("order = %+v", all)
	}
	if all[1].SourceKey != "s3" || !all[1].Completed {
		t.Fatalf("completed item = %+v", all[1])
	}

	unfinished, err := s.History(true, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(unfinished) != 2 {
		t.Fatalf("unfinished = %d: %+v", len(unfinished), unfinished)
	}
	if unfinished[0].SourceKey != "s2" || unfinished[1].SourceKey != "s1" {
		t.Fatalf("unfinished order = %+v", unfinished)
	}
	if unfinished[1].EpisodeID != "l1e4" {
		t.Fatalf("overwrite failed: %+v", unfinished[1])
	}
}

func TestProgressDelete(t *testing.T) {
	s, _ := newStore(t)
	_, _ = s.PutProgress(Progress{SourceKey: "s1", VodID: "1", EpisodeID: "l1e1", Position: 10}, time.Now())
	found, err := s.DeleteProgress("s1:1")
	if err != nil || !found {
		t.Fatalf("delete = %v, %v", found, err)
	}
	found, err = s.DeleteProgress("s1:1")
	if err != nil || found {
		t.Fatalf("second delete = %v, %v", found, err)
	}
	items, _ := s.History(false, 0)
	if len(items) != 0 {
		t.Fatalf("history after delete = %+v", items)
	}
}

func TestProgressClamp(t *testing.T) {
	s, _ := newStore(t)
	saved, err := s.PutProgress(Progress{
		SourceKey: "s1", VodID: "1", EpisodeID: "l1e1", Position: 999, Duration: 100,
	}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if saved.Position != 100 {
		t.Fatalf("position = %v", saved.Position)
	}
	saved, err = s.PutProgress(Progress{
		SourceKey: "s1", VodID: "2", EpisodeID: "l1e1", Position: -5, Duration: -1,
	}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if saved.Position != 0 || saved.Duration != 0 {
		t.Fatalf("negative = %+v", saved)
	}
}

func TestFavorites(t *testing.T) {
	s, _ := newStore(t)
	base := time.Unix(2000, 0)
	f1, err := s.PutFavorite(Favorite{
		SourceKey: "s1", SourceName: "源1", VodID: "1", Title: "影片A", Cover: "https://img/a.jpg",
	}, base)
	if err != nil {
		t.Fatal(err)
	}
	if f1.ID != "s1:1" || f1.CreatedAt != base.UnixMilli() || f1.UpdatedAt != base.UnixMilli() {
		t.Fatalf("favorite = %+v", f1)
	}
	// 更新快照保留 createdAt。
	f1b, err := s.PutFavorite(Favorite{
		SourceKey: "s1", VodID: "1", Title: "影片A（新）", Cover: "https://img/a2.jpg",
	}, base.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if f1b.CreatedAt != base.UnixMilli() || f1b.Title != "影片A（新）" {
		t.Fatalf("updated favorite = %+v", f1b)
	}
	_, _ = s.PutFavorite(Favorite{SourceKey: "s2", VodID: "2", Title: "影片B"}, base.Add(2*time.Minute))

	items, err := s.Favorites(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 || items[0].SourceKey != "s2" {
		t.Fatalf("favorites = %+v", items)
	}
	if _, err := s.DeleteFavorite("s1:1"); err != nil {
		t.Fatal(err)
	}
	items, _ = s.Favorites(0)
	if len(items) != 1 || items[0].SourceKey != "s2" {
		t.Fatalf("after delete = %+v", items)
	}
}

func TestPersistenceAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "userdata.db")
	s, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = s.PutProgress(Progress{SourceKey: "s1", VodID: "1", EpisodeID: "l1e1", Position: 42, Duration: 100}, time.Now())
	_, _ = s.PutFavorite(Favorite{SourceKey: "s1", VodID: "1", Title: "A"}, time.Now())
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s2, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	items, err := s2.History(false, 0)
	if err != nil || len(items) != 1 || items[0].Position != 42 {
		t.Fatalf("progress after reopen = %+v, %v", items, err)
	}
	favs, err := s2.Favorites(0)
	if err != nil || len(favs) != 1 || favs[0].Title != "A" {
		t.Fatalf("favorites after reopen = %+v, %v", favs, err)
	}
}

func TestNewRequiresPath(t *testing.T) {
	if _, err := New(""); err == nil {
		t.Fatal("empty path should fail")
	}
}
