# cms 测试 fixture

除特别说明外均为真实源站响应，抓取于 2026-10-24（经本机代理，请求头同 `DefaultUserAgent`）。
测试只读本地文件，不联网。

| 文件 | 来源 | 说明 |
| --- | --- | --- |
| `search_lz.json` | `https://cj.lziapi.com/api.php/provide/vod/?ac=videolist&wd=庆余年&pg=1` | 量子资源搜索响应，原样保留（3 条，`vod_id` 为字符串） |
| `search_ffzy.json` | `https://cj.ffzyapi.com/api.php/provide/vod/?ac=videolist&wd=庆余年&pg=1` | 非凡资源搜索响应，仅把 `list` 裁到 2 条以控制体积；`vod_id`/`page`/`pagecount` 为数字 |
| `detail_lz.json` | `https://cj.lziapi.com/api.php/provide/vod/?ac=videolist&ids=88770` | 量子资源详情响应：`vod_play_url` 有两条线路，第一条是 `/share/` 中转页、第二条才是 m3u8 |
| `detail_page_ffzy.html` | 手工构造 | 播放列表结构参照非凡影视真实详情页（同一地址在 input/font/onclick 中重复出现）；真实模板没有 h1/sketch，按 MacCMS 常见模板补齐标题与简介 |
