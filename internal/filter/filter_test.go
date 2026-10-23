package filter

import (
	"reflect"
	"testing"

	"cilicili/internal/model"
)

func listItems(names ...string) []model.SearchResultItem {
	items := make([]model.SearchResultItem, 0, len(names))
	for i, name := range names {
		items = append(items, model.SearchResultItem{
			SourceKey:  "src",
			SourceName: "测试源",
			VodID:      string(rune('1' + i)),
			Name:       name,
		})
	}
	return items
}

func namesOf(items []model.SearchResultItem) []string {
	names := make([]string, len(items))
	for i, item := range items {
		names[i] = item.Name
	}
	return names
}

// ---------------------------------------------------------------- NormalizeTitle

func TestNormalizeTitle(t *testing.T) {
	cases := map[string]string{
		"":              "",
		"   ":           "",
		"庆余年 第二季":       "庆余年第二季",
		"摔跤吧！爸爸":        "摔跤吧爸爸",
		"Hello, World!": "helloworld",
		"Mr. Robot":     "mrrobot",
		"《庆余年》":         "庆余年",
		"A_B-C":         "abc",
		"★☆♫":           "",
		"第 1 季":         "第1季",
		"异形：契约":         "异形契约",
		"3.5亿":          "35亿",
	}
	for input, want := range cases {
		if got := NormalizeTitle(input); got != want {
			t.Errorf("NormalizeTitle(%q) = %q, want %q", input, got, want)
		}
	}
}

// ---------------------------------------------------------------- 成人过滤

func TestAdult(t *testing.T) {
	adultTypes := []string{
		"伦理片", "福利片", "福利视频", "里番动漫", "门事件", "擦边短剧", "无码高清",
		"日本有码", "SWAG传媒", "Cosplay", "自拍偷拍", "同性片", "91视频", "mini传媒",
		"日本无码", "有码高清", "黑丝诱惑", "制服诱惑", "国产传媒", "网红主播", "色情片",
		"擦边",
	}
	build := func(types ...string) []model.SearchResultItem {
		items := make([]model.SearchResultItem, 0, len(types))
		for i, typeName := range types {
			items = append(items, model.SearchResultItem{
				SourceKey: "src",
				VodID:     string(rune('1' + i)),
				Name:      "标题",
				TypeName:  typeName,
			})
		}
		return items
	}

	t.Run("全部过滤", func(t *testing.T) {
		out := Adult(build(adultTypes...), true)
		if len(out) != 0 {
			t.Fatalf("len = %d, want 0，未过滤: %v", len(out), namesOf(out))
		}
		if out == nil {
			t.Fatal("应返回非 nil 切片")
		}
	})

	t.Run("保留普通分类", func(t *testing.T) {
		normal := []string{"国产剧", "电影", "动漫", "纪录片", "动作片", "短剧", ""}
		out := Adult(build(normal...), true)
		if !reflect.DeepEqual(namesOf(out), []string{"标题", "标题", "标题", "标题", "标题", "标题", "标题"}) {
			t.Fatalf("out = %#v", out)
		}
		if len(out) != len(normal) {
			t.Fatalf("len = %d, want %d", len(out), len(normal))
		}
	})

	t.Run("保持原顺序", func(t *testing.T) {
		items := []model.SearchResultItem{
			{Name: "a", TypeName: "国产剧"},
			{Name: "b", TypeName: "伦理片"},
			{Name: "c", TypeName: "电影"},
			{Name: "d", TypeName: "福利视频"},
			{Name: "e", TypeName: "动漫"},
		}
		out := Adult(items, true)
		if !reflect.DeepEqual(namesOf(out), []string{"a", "c", "e"}) {
			t.Fatalf("out = %v", namesOf(out))
		}
	})

	t.Run("关闭时原样返回", func(t *testing.T) {
		items := build("伦理片", "国产剧")
		out := Adult(items, false)
		if len(out) != 2 || out[0].TypeName != "伦理片" {
			t.Fatalf("out = %#v", out)
		}
		if &out[0] != &items[0] {
			t.Fatal("关闭过滤时应直接返回原切片")
		}
	})

	t.Run("不修改输入", func(t *testing.T) {
		items := build("伦理片", "国产剧")
		_ = Adult(items, true)
		if len(items) != 2 {
			t.Fatalf("输入切片被修改: %#v", items)
		}
	})
}

// ---------------------------------------------------------------- 相关性

func TestRelevant(t *testing.T) {
	cases := []struct {
		name  string
		wd    string
		items []string
		want  []string
	}{
		{
			name:  "基础包含",
			wd:    "庆余年",
			items: []string{"庆余年", "庆余年第二季", "雪中悍刀行", "庆余年[电影解说]"},
			want:  []string{"庆余年", "庆余年第二季", "庆余年[电影解说]"},
		},
		{
			name:  "标点差异",
			wd:    "摔跤吧！爸爸",
			items: []string{"摔跤吧!爸爸", "摔跤吧！爸爸", "爸爸去哪儿"},
			want:  []string{"摔跤吧!爸爸", "摔跤吧！爸爸"},
		},
		{
			name:  "英文忽略大小写与标点",
			wd:    "mr robot",
			items: []string{"Mr. Robot", "机器人先生"},
			want:  []string{"Mr. Robot"},
		},
		{
			name:  "多词需每个都命中",
			wd:    "庆余年 第二季",
			items: []string{"庆余年第二季", "庆余年 第二季", "庆余年", "第二季"},
			want:  []string{"庆余年第二季", "庆余年 第二季"},
		},
		{
			name:  "裸数字季数别名",
			wd:    "庆余年2",
			items: []string{"庆余年第二季", "庆余年 2", "庆余年2", "庆余年", "庆余年第三季", "庆余年12", "庆余年2019"},
			want:  []string{"庆余年第二季", "庆余年 2", "庆余年2"},
		},
		{
			name:  "中文季数后缀",
			wd:    "庆余年第二季",
			items: []string{"庆余年第二季", "庆余年 第二季", "庆余年第二季粤语", "庆余年2", "庆余年第三季", "庆余年"},
			want:  []string{"庆余年第二季", "庆余年 第二季", "庆余年第二季粤语"},
		},
		{
			name:  "第十季",
			wd:    "庆余年第十季",
			items: []string{"庆余年第十季", "庆余年 第10季", "庆余年第十二季"},
			want:  []string{"庆余年第十季", "庆余年 第10季"},
		},
		{
			name:  "第十二季",
			wd:    "庆余年第十二季",
			items: []string{"庆余年第十二季", "庆余年 第十二季", "庆余年第十一季"},
			want:  []string{"庆余年第十二季", "庆余年 第十二季"},
		},
		{
			name:  "季数附注",
			wd:    "庆余年2",
			items: []string{"庆余年第二季【影视解说】", "庆余年第二季粤语", "庆余年第一季"},
			want:  []string{"庆余年第二季【影视解说】", "庆余年第二季粤语"},
		},
		{
			name:  "数字必须完整命中",
			wd:    "海贼王2",
			items: []string{"海贼王2", "海贼王20", "海贼王12", "海贼王 第2季"},
			want:  []string{"海贼王2", "海贼王 第2季"},
		},
		{
			name:  "纯数字关键词",
			wd:    "12",
			items: []string{"第12号", "120", "12集"},
			want:  []string{"第12号", "12集"},
		},
		{
			name:  "空名称被丢弃",
			wd:    "庆余年",
			items: []string{"", "庆余年"},
			want:  []string{"庆余年"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := Relevant(listItems(tc.items...), tc.wd)
			if got := namesOf(out); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("wd=%q got %v, want %v", tc.wd, got, tc.want)
			}
		})
	}
}

func TestRelevantEmptyKeywordReturnsSameSlice(t *testing.T) {
	for _, wd := range []string{"", "   ", "！！！《》···"} {
		items := listItems("庆余年", "雪中悍刀行")
		out := Relevant(items, wd)
		if &out[0] != &items[0] || len(out) != len(items) {
			t.Fatalf("wd=%q 应原样返回", wd)
		}
	}
}

// ---------------------------------------------------------------- 排序

func TestSortExactFirst(t *testing.T) {
	items := listItems("庆余年第二季", "雪中悍刀行", "庆余年", "庆余年 第二季")
	Sort(items, "庆余年！")
	want := []string{"庆余年", "庆余年 第二季", "庆余年第二季", "雪中悍刀行"}
	if got := namesOf(items); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestSortPinyin(t *testing.T) {
	items := listItems("雪中悍刀行", "变形金刚", "爱情公寓")
	Sort(items, "")
	want := []string{"爱情公寓", "变形金刚", "雪中悍刀行"}
	if got := namesOf(items); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestSortSourceNameTie(t *testing.T) {
	items := []model.SearchResultItem{
		{Name: "斗罗大陆", SourceName: "腾讯视频"},
		{Name: "斗罗大陆", SourceName: "爱奇艺源"},
		{Name: "斗罗大陆", SourceName: "B站源"},
	}
	Sort(items, "斗罗大陆")
	want := []string{"爱奇艺源", "B站源", "腾讯视频"}
	got := make([]string, len(items))
	for i, item := range items {
		got[i] = item.SourceName
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestSortStableOnTie(t *testing.T) {
	items := []model.SearchResultItem{
		{Name: "同一部剧", SourceName: "同一源", VodID: "1"},
		{Name: "同一部剧", SourceName: "同一源", VodID: "2"},
	}
	Sort(items, "同一部剧")
	if items[0].VodID != "1" || items[1].VodID != "2" {
		t.Fatalf("排序不稳定: %#v", items)
	}
}

func TestSortEdges(t *testing.T) {
	Sort(nil, "庆余年")
	Sort([]model.SearchResultItem{}, "庆余年")
	items := listItems("只有一个")
	Sort(items, "只有一个")
	if items[0].Name != "只有一个" {
		t.Fatalf("items = %#v", items)
	}
}

func TestSortKey(t *testing.T) {
	cases := map[string]string{
		"":          "",
		"庆余年":       "qing yu nian ",
		"庆余年 第二季":   "qing yu nian di er ji ",
		"爱情公寓":      "ai qing gong yu ",
		"变形金刚":      "bian xing jin gang ",
		"Mr. Robot": "mrrobot",
		"庆余年2":      "qing yu nian 2",
	}
	for input, want := range cases {
		if got := sortKey(input); got != want {
			t.Errorf("sortKey(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestSortEnglish(t *testing.T) {
	items := listItems("Beta", "alpha", "Gamma")
	Sort(items, "")
	want := []string{"alpha", "Beta", "Gamma"}
	if got := namesOf(items); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

// ---------------------------------------------------------------- 组合流程

func TestSearchPipeline(t *testing.T) {
	items := []model.SearchResultItem{
		{SourceName: "量子资源", Name: "庆余年第二季", TypeName: "国产剧"},
		{SourceName: "非凡资源", Name: "庆余年", TypeName: "国产剧"},
		{SourceName: "量子资源", Name: "庆余年[电影解说]", TypeName: "电影解说"},
		{SourceName: "某源", Name: "庆余年第二季", TypeName: "伦理片"},
		{SourceName: "量子资源", Name: "雪中悍刀行", TypeName: "国产剧"},
	}

	list := Adult(items, true)
	list = Relevant(list, "庆余年")
	Sort(list, "庆余年")

	want := []string{"庆余年", "庆余年第二季", "庆余年[电影解说]"}
	if got := namesOf(list); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}
