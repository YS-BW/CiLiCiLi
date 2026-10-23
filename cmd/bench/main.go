// Command bench 直接调用搜索管线，测量首源/软截止/完成时间。
// 不依赖 HTTP 服务，便于在任意机器（含 VPS）上对比源站延迟。
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"cilicili/internal/cache"
	"cilicili/internal/model"
	"cilicili/internal/search"
	"cilicili/internal/source"
	"cilicili/internal/upstream"
)

type sourcesFile struct {
	Sources []model.SourceConfig `json:"sources"`
}

type runResult struct {
	Keyword   string         `json:"keyword"`
	Run       int            `json:"run"`
	FirstMS   float64        `json:"firstMs"`
	PartialMS float64        `json:"partialMs,omitempty"`
	DoneMS    float64        `json:"doneMs"`
	Count     int            `json:"count"`
	Failures  int            `json:"failures"`
	Sources   int            `json:"sources"`
	Detail    []sourceTiming `json:"detail,omitempty"`
}

type sourceTiming struct {
	Key   string  `json:"key"`
	OK    bool    `json:"ok"`
	MS    int64   `json:"ms"`
	AtMS  float64 `json:"atMs"`
	Error string  `json:"error,omitempty"`
}

func main() {
	sourcesPath := flag.String("sources", "configs/sources.json", "sources json file")
	keywords := flag.String("keywords", "庆余年", "comma separated keywords")
	runs := flag.Int("runs", 3, "runs per keyword")
	jsonOut := flag.Bool("json", false, "print json only")
	warm := flag.Bool("warm", false, "warm sources before measuring")
	verbose := flag.Bool("verbose", false, "print per-source timing")
	flag.Parse()

	data, err := os.ReadFile(*sourcesPath)
	if err != nil {
		fatal(err)
	}
	var sf sourcesFile
	if err := json.Unmarshal(data, &sf); err != nil {
		fatal(err)
	}
	if len(sf.Sources) == 0 {
		fatal(fmt.Errorf("no sources in %s", *sourcesPath))
	}

	client, err := upstream.New(upstream.Config{
		UserAgent:             "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/122.0.0.0 Safari/537.36",
		MaxIdleConns:          128,
		MaxIdleConnsPerHost:   8,
		IdleConnTimeout:       90 * time.Second,
		ConnectTimeout:        3 * time.Second,
		TLSHandshakeTimeout:   3 * time.Second,
		ResponseHeaderTimeout: 5 * time.Second,
	})
	if err != nil {
		fatal(err)
	}
	dir, err := os.MkdirTemp("", "cilici-bench-*")
	if err != nil {
		fatal(err)
	}
	defer os.RemoveAll(dir)
	store, err := cache.New(cache.Config{Path: dir + "/cache.db", MaxMemoryEntries: 4096})
	if err != nil {
		fatal(err)
	}
	defer store.Close()
	health, err := source.NewHealthStore(source.HealthConfig{Path: dir + "/health.db"})
	if err != nil {
		fatal(err)
	}
	defer health.Close()

	searcher := search.New(client, store, health, nil, search.Config{
		MaxPages: 5, SourceTimeout: 6 * time.Second, PageTimeout: 5 * time.Second,
		SoftDeadline: 1200 * time.Millisecond, HardDeadline: 6 * time.Second,
		CacheFresh: 5 * time.Minute, CacheStale: time.Hour,
		SourceFresh: 30 * time.Minute, SourceStale: 6 * time.Hour,
		FailCacheTTL: time.Minute,
	})

	if *warm {
		fmt.Println("warming sources...")
		searcher.Warm(context.Background(), sf.Sources)
	}

	var results []runResult
	for _, kw := range strings.Split(*keywords, ",") {
		kw = strings.TrimSpace(kw)
		if kw == "" {
			continue
		}
		for i := 0; i < *runs; i++ {
			res, err := measure(searcher, kw, sf.Sources)
			if err != nil {
				fatal(err)
			}
			res.Run = i + 1
			results = append(results, res)
			if *verbose {
				sort.Slice(res.Detail, func(i, j int) bool { return res.Detail[i].AtMS < res.Detail[j].AtMS })
				for _, d := range res.Detail {
					fmt.Printf("    %-10s ok=%-5v at=%6.0fms ms=%-5d %s\n", d.Key, d.OK, d.AtMS, d.MS, d.Error)
				}
			}
			if !*jsonOut {
				fmt.Printf("%-10s run=%d first=%6.0fms partial=%6.0fms done=%6.0fms count=%-4d failures=%d\n",
					res.Keyword, res.Run, res.FirstMS, res.PartialMS, res.DoneMS, res.Count, res.Failures)
			}
		}
	}
	if *jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(results)
	} else {
		summary(results)
	}
}

func measure(s *search.Searcher, keyword string, sources []model.SourceConfig) (runResult, error) {
	start := time.Now()
	res := runResult{Keyword: keyword}
	var first, partial float64
	var detail []sourceTiming
	_, err := s.Stream(context.Background(), keyword, sources, true, func(ev any) error {
		now := float64(time.Since(start).Microseconds()) / 1000
		switch e := ev.(type) {
		case model.SourceEvent:
			res.Sources++
			if e.OK && first == 0 {
				first = now
			}
			detail = append(detail, sourceTiming{Key: e.SourceKey, OK: e.OK, MS: e.MS, AtMS: now, Error: e.Error})
		case model.PartialDoneEvent:
			if partial == 0 {
				partial = now
			}
		case model.DoneEvent:
			res.Count = len(e.List)
			res.Failures = len(e.Failures)
			res.DoneMS = now
		}
		return nil
	})
	if err != nil {
		return res, err
	}
	res.FirstMS = first
	res.PartialMS = partial
	res.Detail = detail
	return res, nil
}

func summary(results []runResult) {
	fmt.Println("\n=== summary ===")
	byKeyword := map[string][]runResult{}
	for _, r := range results {
		byKeyword[r.Keyword] = append(byKeyword[r.Keyword], r)
	}
	keys := make([]string, 0, len(byKeyword))
	for k := range byKeyword {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		rs := byKeyword[k]
		cold := rs[0]
		done := make([]float64, 0, len(rs))
		for _, r := range rs {
			done = append(done, r.DoneMS)
		}
		sort.Float64s(done)
		cachedDone := -1.0
		if len(rs) > 1 {
			cachedDone = rs[1].DoneMS
		}
		fmt.Printf("%-10s cold first=%.0fms partial=%.0fms done=%.0fms count=%d; cached done=%.1fms\n",
			k, cold.FirstMS, cold.PartialMS, cold.DoneMS, cold.Count, cachedDone)
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "bench:", err)
	os.Exit(1)
}
