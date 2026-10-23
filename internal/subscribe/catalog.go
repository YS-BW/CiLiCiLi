package subscribe

import "cilicili/internal/model"

// parseCatalog 解析接口目录：裸数组 `[{name?, baseUrl, enabled?}]`，仅点播源。
// 与 LibreTV 裸数组（条目用 url）靠字段区分；enabled 缺省视为启用。
// 跳过与截断都计入 stats，全部不可导入时报错并带上原因明细。
func parseCatalog(entries []any) (*model.SourceListPayload, error) {
	skipped := newSkipCounter()
	sources := make([]model.SourceConfig, 0, min(len(entries), MaxVodSources))
	seen := make(map[string]struct{}, len(entries))
	truncated := 0

	for _, raw := range entries {
		entry, ok := asObject(raw)
		if !ok {
			skipped.mark(model.SkipInvalidURL, "")
			continue
		}
		name := optionalString(entry["name"])

		// 发布者主动停用：不是异常，但仍要计数，否则「目录里 20 条只导入 12 条」无从解释。
		if enabled, isBool := entry["enabled"].(bool); isBool && !enabled {
			skipped.mark(model.SkipDisabled, name)
			continue
		}
		target := NormalizeURL(optionalString(entry["baseUrl"]), true)
		if target == "" {
			skipped.mark(model.SkipInvalidURL, name)
			continue
		}
		if _, duplicate := seen[target]; duplicate {
			continue
		}
		if len(sources) >= MaxVodSources {
			truncated++
			continue
		}
		seen[target] = struct{}{}
		sources = append(sources, model.SourceConfig{
			Key:  sourceKey(target),
			Name: displayName(name, target),
			URL:  target,
		})
	}

	if len(sources) == 0 {
		if skipped.count > 0 {
			return nil, contentErrorf("配置中没有可导入的接口源：已跳过 %d 个条目（%s）",
				skipped.count, describeReasons(skipped.byReason))
		}
		return nil, contentErrorf("配置中没有可用的接口源")
	}

	return &model.SourceListPayload{
		// 裸数组没有列表名：留空由订阅侧用主机名兜底，比硬编一个名字更诚实。
		Sources:     sources,
		LiveSources: make([]model.LiveSourceConfig, 0),
		Stats:       skipped.fill(newStats(formatCatalog), truncated),
	}, nil
}
