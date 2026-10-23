package live

import (
	"errors"
	"sort"
	"testing"
)

func TestParseM3UStandard(t *testing.T) {
	t.Parallel()

	data := "\uFEFF#EXTM3U\r\n" +
		"#PLAYLIST:我的直播\r\n" +
		"#EXTINF:-1 tvg-id=\"cctv1\" tvg-name=\"CCTV-1\" tvg-logo=\"https://logo.example.com/cctv1.png\" group-title=\"央视\",CCTV-1 综合\r\n" +
		"https://live.example.com/cctv1.m3u8\r\n" +
		"#EXTINF:-1 group-title=\"卫视\",湖南卫视\r\n" +
		"https://live.example.com/hunan/index.m3u8\r\n" +
		"#EXTINF:-1 group-title=\"央视\",CCTV-2\r\n" +
		"https://live.example.com/cctv2.m3u8\r\n"

	res, err := ParseM3U([]byte(data))
	if err != nil {
		t.Fatalf("ParseM3U 失败: %v", err)
	}
	if res.Name != "我的直播" {
		t.Errorf("Name = %q，期望 %q", res.Name, "我的直播")
	}
	if len(res.Channels) != 3 {
		t.Fatalf("频道数 = %d，期望 3", len(res.Channels))
	}

	first := res.Channels[0]
	if first.ID != "cctv1" || first.TvgID != "cctv1" {
		t.Errorf("ID/TvgID = %q/%q，期望均为 cctv1", first.ID, first.TvgID)
	}
	if first.Name != "CCTV-1 综合" {
		t.Errorf("Name = %q", first.Name)
	}
	if first.RawName != "CCTV-1" {
		t.Errorf("RawName = %q", first.RawName)
	}
	if first.Logo != "https://logo.example.com/cctv1.png" {
		t.Errorf("Logo = %q", first.Logo)
	}
	if first.Group != "央视" {
		t.Errorf("Group = %q", first.Group)
	}

	wantGroups := []string{"央视", "卫视"}
	sort.Strings(wantGroups)
	if len(res.Groups) != len(wantGroups) {
		t.Fatalf("Groups = %v，期望 %v", res.Groups, wantGroups)
	}
	for i := range wantGroups {
		if res.Groups[i] != wantGroups[i] {
			t.Fatalf("Groups = %v，期望 %v", res.Groups, wantGroups)
		}
	}
}

func TestParseM3UNonStandardAttrsAfterTitle(t *testing.T) {
	t.Parallel()

	data := `#EXTM3U
#EXTINF:-1,CCTV-1 tvg-id="cctv1" tvg-logo="http://logo.example.com/1.png" group-title="央视"
http://live.example.com/1.m3u8
`
	res, err := ParseM3U([]byte(data))
	if err != nil {
		t.Fatalf("ParseM3U 失败: %v", err)
	}
	if len(res.Channels) != 1 {
		t.Fatalf("频道数 = %d", len(res.Channels))
	}
	ch := res.Channels[0]
	if ch.Name != "CCTV-1" || ch.ID != "cctv1" || ch.Group != "央视" || ch.Logo != "http://logo.example.com/1.png" {
		t.Fatalf("解析结果不符合预期: %+v", ch)
	}
}

func TestParseM3UExtGrpAndStableID(t *testing.T) {
	t.Parallel()

	data := `#EXTM3U
#EXTINF:-1,东方卫视
#EXTGRP:上海
https://live.example.com/dongfang.m3u8
`
	res, err := ParseM3U([]byte(data))
	if err != nil {
		t.Fatalf("ParseM3U 失败: %v", err)
	}
	if len(res.Channels) != 1 {
		t.Fatalf("频道数 = %d", len(res.Channels))
	}
	ch := res.Channels[0]
	if ch.Group != "上海" {
		t.Errorf("Group = %q，期望 上海", ch.Group)
	}
	if ch.ID == "" {
		t.Fatal("缺少 tvg-id 时应生成稳定 hash id")
	}

	again, err := ParseM3U([]byte(data))
	if err != nil {
		t.Fatalf("ParseM3U 失败: %v", err)
	}
	if again.Channels[0].ID != ch.ID {
		t.Errorf("稳定 id 不一致: %q vs %q", again.Channels[0].ID, ch.ID)
	}
	// djb2("https://live.example.com/dongfang.m3u8") 的固定值，回归保护。
	const wantID = "8dc6ad"
	if ch.ID != wantID {
		t.Errorf("id = %q，期望 %q", ch.ID, wantID)
	}

	// 空串同样走 djb2：h=5381 → base36 "45h"（与 LibreTV 一致）。
	if got := stableID(""); got != "45h" {
		t.Errorf("stableID(\"\") = %q，期望 45h", got)
	}
}

func TestParseM3UFiltersAndDedupes(t *testing.T) {
	t.Parallel()

	data := `#EXTM3U
#EXTINF:-1,央视
rtp://239.0.0.1:5000
#EXTINF:-1,组播
udp://239.0.0.2:5000
#EXTINF:-1,相对地址
live/stream.m3u8
#EXTINF:-1,重复频道
https://live.example.com/dup.m3u8
#EXTINF:-1,重复频道2
https://live.example.com/dup.m3u8
#EXTINF:-1,
https://live.example.com/path/channel.flv
`
	res, err := ParseM3U([]byte(data))
	if err != nil {
		t.Fatalf("ParseM3U 失败: %v", err)
	}
	if len(res.Channels) != 2 {
		t.Fatalf("频道数 = %d，期望 2（只保留 http(s) 且去重）: %+v", len(res.Channels), res.Channels)
	}
	if res.Channels[0].URL != "https://live.example.com/dup.m3u8" {
		t.Errorf("URL = %q", res.Channels[0].URL)
	}
	if res.Channels[1].Name != "channel" {
		t.Errorf("无名称频道的兜底名称 = %q，期望 channel", res.Channels[1].Name)
	}
}

func TestParseM3UNameFallbackFromHost(t *testing.T) {
	t.Parallel()

	data := `#EXTM3U
https://only-host.example.com/
`
	res, err := ParseM3U([]byte(data))
	if err != nil {
		t.Fatalf("ParseM3U 失败: %v", err)
	}
	if len(res.Channels) != 1 {
		t.Fatalf("频道数 = %d", len(res.Channels))
	}
	if res.Channels[0].Name != "only-host.example.com" {
		t.Errorf("Name = %q", res.Channels[0].Name)
	}
}

func TestParseM3URejectsInvalid(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		data string
	}{
		{"空内容", ""},
		{"只有空白", "  \n\t\r\n"},
		{"HTML 错误页", "<html><body>404 Not Found</body></html>"},
		{"JSON 错误页", `{"error":"invalid token"}`},
		{"缺少 E X T M 3 U", "#EXTINF:-1,频道\nhttps://live.example.com/a.m3u8\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := ParseM3U([]byte(tc.data)); !errors.Is(err, ErrInvalidPlaylist) {
				t.Fatalf("err = %v，期望 ErrInvalidPlaylist", err)
			}
		})
	}
}

func TestParseM3UNoChannels(t *testing.T) {
	t.Parallel()

	res, err := ParseM3U([]byte("#EXTM3U\n#EXTINF:-1,只有注释\n"))
	if err != nil {
		t.Fatalf("ParseM3U 失败: %v", err)
	}
	if res.Channels == nil || len(res.Channels) != 0 {
		t.Errorf("Channels = %v，期望空切片而不是 nil", res.Channels)
	}
	if res.Groups == nil {
		t.Error("Groups 不应为 nil")
	}
}

func TestParseM3UWithBaseURL(t *testing.T) {
	t.Parallel()

	data := `#EXTM3U
#EXTINF:-1 tvg-logo="logo.png",频道
stream/ch.m3u8
`
	res, err := parseM3U([]byte(data), "https://example.com/live/list.m3u")
	if err != nil {
		t.Fatalf("parseM3U 失败: %v", err)
	}
	if len(res.Channels) != 1 {
		t.Fatalf("频道数 = %d", len(res.Channels))
	}
	if got := res.Channels[0].URL; got != "https://example.com/live/stream/ch.m3u8" {
		t.Errorf("URL = %q", got)
	}
	if got := res.Channels[0].Logo; got != "https://example.com/live/logo.png" {
		t.Errorf("Logo = %q", got)
	}
}

func TestPlaylistName(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"#EXTM3U\n#PLAYLIST: 我的频道 \n": "我的频道",
		"#EXTM3U\n#PLAYLIST:\n":       "",
		"#EXTM3U\n":                   "",
	}
	for data, want := range cases {
		if got := playlistName(data); got != want {
			t.Errorf("playlistName(%q) = %q，期望 %q", data, got, want)
		}
	}
}

func TestNameFromURL(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"https://live.example.com/a/b/CCTV1.m3u8?token=x": "CCTV1",
		"https://live.example.com/%E6%B9%96%E5%8D%97.ts":  "湖南",
		"https://live.example.com/":                       "live.example.com",
		"not a url":                                       "未命名频道",
	}
	for raw, want := range cases {
		if got := nameFromURL(raw); got != want {
			t.Errorf("nameFromURL(%q) = %q，期望 %q", raw, got, want)
		}
	}
}
