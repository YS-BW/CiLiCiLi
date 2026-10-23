# 源配置

`sources.json` 是部署预置源，由 `SOURCES_FILE` 指向。

- 点播源是第三方 Apple CMS 采集站，**随时可能失效**；服务端会按健康度自动熔断。
- 已剔除成人聚合站 `ckzy`（交接文档建议）。
- `key` 必须稳定且唯一：服务端健康状态按 key 持久化，换 key 等于换一个源。
- 也可以继续用 `DEFAULT_SOURCES` 环境变量（LibreTV 兼容），但推荐文件方式。
- 直播源只放 M3U 订阅地址；EPG 可选。
