package sources

import (
	"errors"
	"path/filepath"
	"testing"

	"cilicili/internal/model"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := New(filepath.Join(t.TempDir(), "sources.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestSeedOnlyOnceAndList(t *testing.T) {
	s := newTestStore(t)
	seed := []model.SourceConfig{
		{Key: "a", Name: "源A", URL: "https://a.test/api.php/provide/vod"},
		{Key: "b", Name: "源B", URL: "https://b.test/api.php/provide/vod"},
	}
	if err := s.Seed(seed); err != nil {
		t.Fatal(err)
	}
	// 再次 Seed 不应重复导入。
	if err := s.Seed(seed); err != nil {
		t.Fatal(err)
	}
	list, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].Key != "a" || !list[0].Enabled {
		t.Fatalf("list = %+v", list)
	}
	enabled, _ := s.ListEnabled()
	if len(enabled) != 2 {
		t.Fatalf("enabled = %+v", enabled)
	}
}

func TestCreateUpdateDeleteSource(t *testing.T) {
	s := newTestStore(t)
	src, err := s.Create(Source{SourceConfig: model.SourceConfig{Name: "新源", URL: "https://new.test/api.php/provide/vod/"}})
	if err != nil {
		t.Fatal(err)
	}
	if src.Key == "" || src.URL != "https://new.test/api.php/provide/vod" || !src.Enabled {
		t.Fatalf("created = %+v", src)
	}
	// 重复 URL 冲突。
	if _, err := s.Create(Source{SourceConfig: model.SourceConfig{Name: "重复", URL: src.URL}}); !errors.Is(err, ErrConflict) {
		t.Fatalf("dup err = %v", err)
	}
	// 无效 URL。
	if _, err := s.Create(Source{SourceConfig: model.SourceConfig{Name: "坏", URL: "ftp://x"}}); !errors.Is(err, ErrInvalidURL) {
		t.Fatalf("invalid err = %v", err)
	}

	name := "改名"
	enabled := false
	updated, err := s.Update(src.Key, SourcePatch{Name: &name, Enabled: &enabled})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Name != "改名" || updated.Enabled {
		t.Fatalf("updated = %+v", updated)
	}
	enabledSources, _ := s.ListEnabled()
	if len(enabledSources) != 0 {
		t.Fatalf("disabled source should not be enabled: %+v", enabledSources)
	}

	if err := s.Delete(src.Key); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(src.Key); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get after delete err = %v", err)
	}
	if err := s.Delete(src.Key); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete again err = %v", err)
	}
}

func TestReorder(t *testing.T) {
	s := newTestStore(t)
	for _, key := range []string{"a", "b", "c"} {
		if _, err := s.CreateWithEnabled(Source{SourceConfig: model.SourceConfig{Key: key, Name: key, URL: "https://" + key + ".test/api"}}, true); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Reorder([]string{"c", "a"}); err != nil {
		t.Fatal(err)
	}
	list, _ := s.List()
	got := []string{list[0].Key, list[1].Key, list[2].Key}
	want := []string{"c", "a", "b"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order = %v, want %v", got, want)
		}
	}
}

func TestSubscriptions(t *testing.T) {
	s := newTestStore(t)
	sub, err := s.CreateSubscription("订阅一", "https://sub.test/sources.json/")
	if err != nil {
		t.Fatal(err)
	}
	if sub.ID == 0 || sub.URL != "https://sub.test/sources.json" || !sub.Enabled {
		t.Fatalf("sub = %+v", sub)
	}
	if _, err := s.CreateSubscription("重复", sub.URL); !errors.Is(err, ErrConflict) {
		t.Fatalf("dup err = %v", err)
	}
	if err := s.SetSubscriptionResult(sub.ID, "ok", "", 12); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetSubscription(sub.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.LastStatus != "ok" || got.LastCount != 12 || got.LastFetchAt == 0 {
		t.Fatalf("result = %+v", got)
	}
	disabled := false
	if _, err := s.UpdateSubscription(sub.ID, nil, nil, &disabled); err != nil {
		t.Fatal(err)
	}
	list, err := s.ListSubscriptions()
	if err != nil || len(list) != 1 || list[0].Enabled {
		t.Fatalf("list = %+v err=%v", list, err)
	}
	if err := s.DeleteSubscription(sub.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteSubscription(sub.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete again err = %v", err)
	}
}

func TestNormalizeURLAndKey(t *testing.T) {
	for _, raw := range []string{"", "ftp://x", "http://", "https://a b"} {
		if _, err := NormalizeURL(raw); err == nil {
			t.Fatalf("%q should be invalid", raw)
		}
	}
	got, err := NormalizeURL(" https://a.test/api/?x=1/ ")
	if err != nil || got != "https://a.test/api/?x=1" {
		t.Fatalf("normalize = %q err=%v", got, err)
	}
	if NewKey("https://a.test/api") != NewKey("https://a.test/api") {
		t.Fatal("key should be stable")
	}
	if NewKey("https://a.test/api") == NewKey("https://b.test/api") {
		t.Fatal("different urls should have different keys")
	}
}

func TestDeleteBlocksURLAndReaddUnblocks(t *testing.T) {
	s := newTestStore(t)
	src, err := s.Create(Source{SourceConfig: model.SourceConfig{Name: "自动发现的源", URL: "https://auto.test/api.php/provide/vod"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(src.Key); err != nil {
		t.Fatal(err)
	}
	blocked, err := s.BlockedURLs()
	if err != nil {
		t.Fatal(err)
	}
	if !blocked[src.URL] {
		t.Fatalf("deleted source url should be blocked: %+v", blocked)
	}
	// 手动重新添加同一 URL 会解除黑名单。
	if _, err := s.Create(Source{SourceConfig: model.SourceConfig{Name: "手动加回", URL: src.URL}}); err != nil {
		t.Fatal(err)
	}
	blocked, _ = s.BlockedURLs()
	if blocked[src.URL] {
		t.Fatalf("re-added source should be unblocked: %+v", blocked)
	}
}
