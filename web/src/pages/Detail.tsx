import { useEffect, useMemo, useRef, useState, lazy, Suspense } from 'react';
import { useParams, useSearchParams, useNavigate } from 'react-router-dom';
import { api } from '../api/client';
import type { VideoDetail, PlayLine, Episode, SourceRef } from '../api/types';
import { getSources, rememberSources } from '../lib/sourcesCache';
import { createRoom } from '../lib/watchparty';
import { saveWatchRoomMeta, type WatchRoomMeta } from './WatchRoom';

// 播放器（ArtPlayer + hls.js，约占一半体积）只在真正要播时下载，其它页面首屏 JS 因此减半。
const Player = lazy(() => import('../components/Player'));

export default function Detail() {
  const { sourceKey = '', vodId = '' } = useParams();
  const [params] = useSearchParams();
  const nav = useNavigate();

  const [detail, setDetail] = useState<VideoDetail | null>(null);
  const [loading, setLoading] = useState(true);
  const [err, setErr] = useState('');
  const [lineIdx, setLineIdx] = useState(0);
  const [epIdx, setEpIdx] = useState(0);
  const [playing, setPlaying] = useState(false);
  const [hint, setHint] = useState('');
  const [faved, setFaved] = useState(false);
  const [creatingWatch, setCreatingWatch] = useState(false);
  const [heroBg, setHeroBg] = useState<{ url: string; wide: boolean } | null>(null);
  const [allSources, setAllSources] = useState<SourceRef[]>([]);
  const [showEps, setShowEps] = useState(false);
  const hintTimer = useRef<ReturnType<typeof setTimeout> | null>(null);

  // 同片可切换的采集源：优先用搜索/首页带过来的，否则按标题搜一次补全。
  useEffect(() => {
    if (!detail) return;
    const title = detail.videoInfo?.title || '';
    let cancelled = false;
    const cached = getSources(title);
    if (cached.length > 1) { setAllSources(cached); return; }
    api.search(title, { fast: true }).then(r => {
      if (cancelled) return;
      const m = r.media && r.media[0];
      const list = m?.sources?.length ? m.sources : cached;
      if (list && list.length) { setAllSources(list); rememberSources(title, list); }
    }).catch(() => { if (!cancelled) setAllSources(cached); });
    return () => { cancelled = true; };
  }, [detail]);

  // 详情页大图：预加载 big 封面，按图片朝向决定展示方式（横图才铺满，竖图不拉伸）。
  useEffect(() => {
    if (!detail) return;
    const vi = detail.videoInfo;
    const kind: 'movie' | 'tv' = isTvType(detail) ? 'tv' : 'movie';
    const bigUrl = api.coverUrl({ title: vi.title, year: vi.year, kind, src: vi.cover, big: true });
    let cancelled = false;
    const img = new Image();
    img.onload = () => {
      if (cancelled) return;
      // 宽高比 >= 1.15 视为横版剧照（铺满）；否则是竖版海报（模糊底 + 居中，不拉伸）。
      setHeroBg({ url: bigUrl, wide: img.naturalWidth >= img.naturalHeight * 1.15 });
    };
    img.onerror = () => { if (!cancelled) setHeroBg({ url: '', wide: false }); };
    img.src = bigUrl;
    return () => { cancelled = true; };
  }, [detail]);

  // 拉详情
  useEffect(() => {
    (async () => {
      setLoading(true); setErr('');
      try {
        const d = await api.detail(sourceKey, vodId);
        setDetail(d);
        // 默认选 preferred 线路的第一集；或 URL 指定的 ep
        const pref = Math.max(0, d.playLines.findIndex(l => l.preferred));
        setLineIdx(pref >= 0 ? pref : 0);
        const wantEp = params.get('ep') || '';
        const line = d.playLines[pref >= 0 ? pref : 0];
        let ep = 0;
        if (wantEp && line) {
          const found = line.episodes.findIndex(e => e.id === wantEp);
          if (found >= 0) ep = found;
        }
        setEpIdx(ep);
      } catch (e: any) {
        setErr(e?.message || '详情加载失败');
      } finally {
        setLoading(false);
      }
    })();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [sourceKey, vodId]);

  // 收藏状态
  useEffect(() => {
    (async () => {
      try {
        const favs = await api.favorites();
        setFaved(favs.some(f => f.sourceKey === sourceKey && f.vodId === vodId));
      } catch { /* ignore */ }
    })();
  }, [sourceKey, vodId]);

  const line: PlayLine | undefined = detail?.playLines[lineIdx];
  const episode: Episode | undefined = line?.episodes[epIdx];

  const playUrl = useMemo(() => (playing && episode ? episode.url : ''), [playing, episode]);

  function showHint(text: string) {
    setHint(text);
    if (hintTimer.current) clearTimeout(hintTimer.current);
    hintTimer.current = setTimeout(() => setHint(''), 2800);
  }

  async function restorePosition(): Promise<number> {
    try {
      const hist = await api.history(true, 100);
      const rec = hist.find(h => h.sourceKey === sourceKey && h.vodId === vodId);
      if (!rec) return 0;
      // 优先按 episodeId 匹配同一集，否则同片最近进度
      if (episode && rec.episodeId && rec.episodeId === episode.id) return rec.position;
      if (episode && rec.episodeId === episode.id) return rec.position;
      return rec.position || 0;
    } catch { return 0; }
  }

  function reportProgress(position: number, duration: number) {
    if (!episode || !detail) return;
    if (position <= 0) return;
    const completed = duration > 0 && position >= duration - 5;
    api.saveProgress({
      sourceKey, sourceName: detail.videoInfo.sourceName || '',
      vodId, title: detail.videoInfo.title, cover: detail.videoInfo.cover,
      lineId: line?.id, lineName: line?.name,
      episodeId: episode.id, episodeName: episode.name, episodeIndex: episode.index,
      position, duration, completed,
    }).catch(() => {});
  }

  function nextEpisode() {
    if (!line) return;
    if (epIdx + 1 < line.episodes.length) { setEpIdx(epIdx + 1); showHint('自动播放下一集'); }
  }

  async function toggleFav() {
    try {
      if (faved) {
        await api.removeFavorite(`${sourceKey}:${vodId}`);
        setFaved(false); showHint('已取消收藏');
      } else if (detail) {
        await api.addFavorite({
          sourceKey, sourceName: detail.videoInfo.sourceName || '',
          vodId, title: detail.videoInfo.title, cover: detail.videoInfo.cover || '',
        });
        setFaved(true); showHint('已收藏');
      }
    } catch (e: any) { showHint(e?.message || '操作失败'); }
  }

  async function startWatchTogether() {
    if (!detail || !line || !episode || creatingWatch) {
      showHint('请先选择要播放的剧集');
      return;
    }
    setCreatingWatch(true);
    try {
      const room = await createRoom('房主');
      const meta: WatchRoomMeta = {
        title: detail.videoInfo.title,
        url: episode.url,
        episode: epIdx,
        episodes: line.episodes.map((e, i) => ({ id: e.id, name: e.name, index: i, url: e.url })),
        detailPath: `/play/${encodeURIComponent(sourceKey)}/${encodeURIComponent(vodId)}`,
      };
      saveWatchRoomMeta(room.roomId, meta);
      nav(`/watch/${encodeURIComponent(room.roomId)}`, { state: meta });
    } catch (e: any) {
      showHint(e?.message || '创建一起看房间失败');
    } finally {
      setCreatingWatch(false);
    }
  }

  if (loading) return <div className="center-fill"><div className="spinner" /><div className="muted">加载详情…</div></div>;
  if (err) return <div className="center-fill"><div className="error-box">{err}</div><button className="btn ghost" onClick={() => nav(-1)}>返回</button></div>;
  if (!detail) return null;

  const vi = detail.videoInfo;
  const kind: 'movie' | 'tv' = isTvType(detail) ? 'tv' : 'movie';
  // 模糊底层用原站封面（vi.cover）；清晰前景只用 TMDB 横版大图（big=1）。
  const blurSrc = vi.cover ? api.proxyUrl(vi.cover) : api.coverUrl({ title: vi.title, year: vi.year, kind, big: false });

  return (
    <div>
      <section className="detail-hero wide">
        {/* 底层：原站封面模糊放大打底，任何情况都有底色、不空白、不拉伸 */}
        <div className="dh-blur" style={blurSrc ? { backgroundImage: `url(${blurSrc})` } : undefined} />
        {/* 清晰层：仅当拿到 TMDB 横版剧照时淡入覆盖；没有就保持模糊底（不显示竖版大图） */}
        {heroBg && heroBg.url && heroBg.wide ? (
          <div className="bg-wide dh-fadein" style={{ backgroundImage: `url(${heroBg.url})` }} />
        ) : null}
        <div className="dh-fade" />
        <div className="inner">
          <h1>{vi.title || vodId}</h1>
          <div className="meta">
            {vi.year ? <span>{vi.year}</span> : null}
            {vi.area ? <span>{vi.area}</span> : null}
            {vi.typeName ? <span>{vi.typeName}</span> : null}
            {vi.remarks ? <span>{vi.remarks}</span> : null}
            {vi.sourceName ? <span className="muted">当前源：{vi.sourceName}</span> : null}
          </div>
          <div className="desc">{vi.desc || '暂无简介'}</div>
          <div className="dbtns">
            <button className="btn play" onClick={() => setPlaying(true)}>▶ {episode ? `播放 ${episode.name || `第${episode.index}集`}` : '播放'}</button>
            <button className="btn ghost" onClick={startWatchTogether} disabled={creatingWatch || !episode}>
              {creatingWatch ? '创建中…' : '一起看'}
            </button>
            <button className="btn ghost" onClick={toggleFav}>{faved ? '✓ 已收藏' : '☆ 收藏'}</button>
          </div>
        </div>
      </section>

      {/* 正片内容区：左选集 + 右信息，填满屏幕 */}
      <section className="detail-body">
        <div>
          {allSources.length > 1 && (
            <div className="lines">
              <span>片源</span>
              {allSources.map(s => {
                const cur = s.sourceKey === sourceKey && s.vodId === vodId;
                return (
                  <span key={s.sourceKey + ':' + s.vodId} className={`l${cur ? ' active' : ''}`}
                    onClick={() => { if (!cur) nav(`/play/${encodeURIComponent(s.sourceKey)}/${encodeURIComponent(s.vodId)}`); }}>
                    {s.sourceName || s.sourceKey}
                  </span>
                );
              })}
            </div>
          )}
          {detail.playLines.length > 1 && (
            <div className="lines">
              <span>线路</span>
              {detail.playLines.map((l, i) => (
                <span key={l.id} className={`l${i === lineIdx ? ' active' : ''}`}
                  onClick={() => { setLineIdx(i); setEpIdx(0); }}>{l.name || `线路${i + 1}`}{l.preferred ? ' · 推荐' : ''}</span>
              ))}
            </div>
          )}
          <div className="sec-title">选集{line ? <span className="muted" style={{ fontSize: 14, fontWeight: 400 }}>共 {line.episodes.length} 集</span> : null}</div>
          {line && line.episodes.length > 0 && (
            <div className="eps">
              {line.episodes.map((e, i) => (
                <span key={e.id} className={`ep${i === epIdx ? ' active' : ''}`}
                  onClick={() => { setEpIdx(i); setPlaying(true); }}>
                  {e.name || `第${e.index}集`}
                </span>
              ))}
            </div>
          )}
        </div>
        <aside className="detail-aside">
          <div className="sec-title">详情</div>
          {vi.director ? <div className="row"><span className="k">导演</span><span className="v">{vi.director}</span></div> : null}
          {vi.actor ? <div className="row"><span className="k">主演</span><span className="v clamp">{vi.actor}</span></div> : null}
          {vi.typeName ? <div className="row"><span className="k">类型</span><span className="v">{vi.typeName}</span></div> : null}
          {vi.area ? <div className="row"><span className="k">地区</span><span className="v">{vi.area}</span></div> : null}
          {vi.year ? <div className="row"><span className="k">年份</span><span className="v">{vi.year}</span></div> : null}
          {vi.remarks ? <div className="row"><span className="k">状态</span><span className="v">{vi.remarks}</span></div> : null}
          {vi.sourceName ? <div className="row"><span className="k">片源</span><span className="v">{vi.sourceName}</span></div> : null}
        </aside>
      </section>

      {playing && episode && (
        <div className="nf-player">
          <div className="nf-top">
            <button className="nf-back" onClick={() => setPlaying(false)}>‹ 返回</button>
            <div className="nf-title">{vi.title}{episode.name ? ` · ${episode.name}` : ` · 第${episode.index}集`}</div>
            <div className="nf-top-right">
              <button className="nf-btn" onClick={() => setShowEps(v => !v)}>选集</button>
              <button className="nf-btn" onClick={() => {
                if (!line) return;
                const i = line.episodes.findIndex(e => e.id === episode.id);
                if (i >= 0 && i + 1 < line.episodes.length) setEpIdx(i + 1);
              }}>下一集</button>
            </div>
          </div>
          <div className="nf-video">
            <Suspense fallback={<div className="center-fill"><div className="spinner" /></div>}>
              <Player
                url={playUrl}
                episodeKey={episode.id}
                getRestorePosition={restorePosition}
                onProgress={reportProgress}
                onEnded={nextEpisode}
                onHint={showHint}
                onRequestSwitchSource={() => { showHint('播放失败，请切换线路或片源'); }}
              />
            </Suspense>
          </div>
          {showEps && line ? (
            <div className="nf-eps-panel">
              <div className="nf-eps-head">选集 · {line.name || '线路'}</div>
              <div className="nf-eps-grid">
                {line.episodes.map((e, i) => (
                  <span key={e.id} className={`nf-ep${e.id === episode.id ? ' active' : ''}`}
                    onClick={() => { setEpIdx(i); }}>{e.name || `第${e.index}集`}</span>
                ))}
              </div>
            </div>
          ) : null}
          {hint ? <div className="nf-hint">{hint}</div> : null}
        </div>
      )}

      {hint && !playing ? (
        <div style={{ position: 'fixed', left: '50%', bottom: 30, transform: 'translateX(-50%)', background: 'rgba(0,0,0,.85)', padding: '10px 20px', borderRadius: 20, fontSize: 13, zIndex: 100 }}>{hint}</div>
      ) : null}
    </div>
  );
}
function viTitle(d: VideoDetail): string {
  return (d.videoInfo?.title || '').trim();
}
// 判断是否剧集：注意「喜剧/悲剧/闹剧」都含「剧」字，不能只靠单字「剧」误判成剧集。
// 规则：类型名以「片」结尾或含「电影」→ 电影；含剧集/连续剧/电视剧/综艺/动漫/番剧/动画/集数 → tv。
function isTvType(d: VideoDetail): boolean {
  const type = (d.videoInfo?.typeName || '').trim();
  const remarks = (d.videoInfo?.remarks || '');
  // 电影优先：类型名以「片」结尾（喜剧片/动作片/剧情片…）或含「电影」
  if (/片$/.test(type) || type.includes('电影')) return false;
  const t = `${type} ${remarks}`;
  if (/剧集|连续剧|电视剧|美剧|日剧|韩剧|港剧|台剧|短剧|综艺|动漫|番剧|动画|集/.test(t)) return true;
  if (/剧$/.test(type)) return true; // 国产剧/欧美剧 等以剧结尾
  return false;
}

