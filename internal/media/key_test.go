package media

import "testing"

func TestNormalizeTitle(t *testing.T) {
	cases := []struct {
		in     string
		base   string
		season int
	}{
		{"庆余年", "庆余年", 0},
		{"庆余年 第二季", "庆余年", 2},
		{"庆余年2", "庆余年", 2},
		{"庆余年 第2季", "庆余年", 2},
		{"庆余年 2 (2019)", "庆余年", 2},
		{"The Office Season 2", "theoffice", 2},
		{"Show S02", "show", 2},
		{"电影：你好，李焕英！", "电影你好李焕英", 0},
	}
	for _, tc := range cases {
		base, season := NormalizeTitle(tc.in)
		if base != tc.base || season != tc.season {
			t.Errorf("NormalizeTitle(%q) = (%q,%d), want (%q,%d)", tc.in, base, season, tc.base, tc.season)
		}
	}
}

func TestExtractYear(t *testing.T) {
	for in, want := range map[string]string{
		"2019": "2019", "2019-01-01": "2019", "(2024)": "2024", "": "", "未知": "",
	} {
		if got := ExtractYear(in); got != want {
			t.Errorf("ExtractYear(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestAggregateKey(t *testing.T) {
	a := AggregateKey(KindTV, "庆余年", "2019")
	b := AggregateKey(KindTV, "庆余年 第二季", "2024")
	if a == "" || b == "" || a == b {
		t.Fatalf("keys = %q, %q", a, b)
	}
	if AggregateKey(KindTV, "庆余年", "") != "" {
		t.Fatal("missing year should not aggregate")
	}
	if AggregateKey(KindOther, "庆余年", "2019") != "" {
		t.Fatal("other kind should not aggregate")
	}
	if AggregateKey(KindTV, "庆余年（2019）", "2019") != a {
		t.Fatal("year in title should normalize to same key")
	}
}

func TestKindFromType(t *testing.T) {
	cases := map[string]Kind{
		"电影": KindMovie, "动作片": KindMovie, "国产剧": KindTV, "电视剧": KindTV,
		"动漫": KindAnime, "番剧": KindAnime, "综艺": KindTV, "": KindOther, "其他": KindOther,
	}
	for in, want := range cases {
		if got := KindFromType(in); got != want {
			t.Errorf("KindFromType(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestScoreCandidate(t *testing.T) {
	req := Request{Kind: KindTV, Title: "庆余年 第二季", Year: "2024"}
	exact := Candidate{Title: "庆余年 第二季", Year: "2024", Kind: KindTV, Provider: "bangumi"}
	score, reasons := ScoreCandidate(req, exact)
	if score < 0.9 || len(reasons) == 0 {
		t.Fatalf("exact score = %v reasons=%v", score, reasons)
	}
	if !isStrongMatch(req, exact) {
		t.Fatal("exact candidate should be strong")
	}
	partial := Candidate{Title: "庆余年", Year: "2024", Kind: KindTV, Provider: "bangumi"}
	if isStrongMatch(req, partial) {
		t.Fatal("partial title should not auto confirm")
	}
	wrongYear := Candidate{Title: "庆余年 第二季", Year: "2020", Kind: KindTV, Provider: "bangumi"}
	if isStrongMatch(req, wrongYear) {
		t.Fatal("wrong year should not auto confirm")
	}
	if s, _ := ScoreCandidate(req, wrongYear); s > score {
		t.Fatal("wrong year should score lower")
	}
}
