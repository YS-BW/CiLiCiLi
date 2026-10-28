import { useEffect, useMemo, useRef, useState } from 'react';
import { useParams, useSearchParams, useNavigate } from 'react-router-dom';
import { api } from '../api/client';
import type { VideoDetail, PlayLine, Episode } from '../api/types';
import Player, { type PlayerHandle } from '../components/Player';

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
  const [backdrop, setBackdrop] = useState('');
  const playerRef = useRef<PlayerHandle | null>(null);
  const hintTimer = useRef<ReturnType<typeof setTimeout> | null>(null);

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
        // 详情页背景大图：按标题查 TMDB 横版剧照（豆瓣/采集站只有竖版封面）
        api.backdrop(viTitle(d), isTvType(d) ? 'tv' : 'movie').then(bd => { if (bd) setBackdrop(bd); }).catch(() => {});
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

  if (loading) return <div className="center-fill"><div className="spinner" /><div className="muted">加载详情…</div></div>;
  if (err) return <div className="center-fill"><div className="error-box">{err}</div><button className="btn ghost" onClick={() => nav(-1)}>返回</button></div>;
  if (!detail) return null;

  const vi = detail.videoInfo;
  // 优先 TMDB 横版剧照；没有则回退竖版封面（模糊铺底 + 底部渐变）
  const bg = backdrop || vi.cover;
  const isWideBg = !!backdrop;

  return (
    <div>
      <section className={`detail-hero${isWideBg ? ' wide' : ''}`}>
        {isWideBg ? (
          <div className="bg-wide" style={{ backgroundImage: `url(${api.proxyUrl(bg)})` }} />
        ) : (
          <>
            {bg ? <div className="bg-blur" style={{ backgroundImage: `url(${api.proxyUrl(bg)})` }} /> : null}
            <div className="bg" style={{ backgroundImage: bg ? `url(${api.proxyUrl(bg)})` : 'radial-gradient(120% 100% at 70% 20%,#2f2418,#050505)' }} />
          </>
        )}
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
            <button className="btn ghost" onClick={toggleFav}>{faved ? '✓ 已收藏' : '☆ 收藏'}</button>
          </div>
        </div>
      </section>

      {/* 正片内容区：左选集 + 右信息，填满屏幕 */}
      <section className="detail-body">
        <div>
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
          {vi.actor ? <div className="row"><span className="k">主演</span><span className="v">{vi.actor}</span></div> : null}
          {vi.typeName ? <div className="row"><span className="k">类型</span><span className="v">{vi.typeName}</span></div> : null}
          {vi.area ? <div className="row"><span className="k">地区</span><span className="v">{vi.area}</span></div> : null}
          {vi.year ? <div className="row"><span className="k">年份</span><span className="v">{vi.year}</span></div> : null}
          {vi.remarks ? <div className="row"><span className="k">状态</span><span className="v">{vi.remarks}</span></div> : null}
          {vi.sourceName ? <div className="row"><span className="k">片源</span><span className="v">{vi.sourceName}</span></div> : null}
        </aside>
      </section>

      {playing && episode && (
        <div className="player-modal" onClick={(e) => { if (e.target === e.currentTarget) setPlaying(false); }}>
          <div className="player-box">
            <button className="player-close" onClick={() => setPlaying(false)}>✕</button>
            <Player
              ref={playerRef}
              url={playUrl}
              episodeKey={episode.id}
              getRestorePosition={restorePosition}
              onProgress={reportProgress}
              onEnded={nextEpisode}
              onHint={showHint}
              onError={(t) => showHint(t)}
              onRequestSwitchSource={() => { showHint('播放失败，请切换线路或片源'); }}
            />
            {hint ? <div className="player-hint">{hint}</div> : null}
          </div>
        </div>
      )}

      {hint && !playing ? (
        <div style={{ position: 'fixed', left: '50%', bottom: 30, transform: 'translateX(-50%)', background: 'rgba(0,0,0,.85)', padding: '10px 20px', borderRadius: 20, fontSize: 13, zIndex: 100 }}>{hint}</div>
      ) : null}
    </div>
  );
}

// 详情页查 backdrop 用的标题：优先 videoInfo.title，去掉尾部「第二季」等季数后缀影响不大，TMDB 能容忍。
function viTitle(d: VideoDetail): string {
  return (d.videoInfo?.title || '').trim();
}
// 粗判类型：剧集/动漫 → tv；其余按 movie。
function isTvType(d: VideoDetail): boolean {
  const t = `${d.videoInfo?.typeName || ''} ${d.videoInfo?.remarks || ''}`;
  return /剧|连续|集|综艺|动漫|番剧|动画/.test(t);
}

