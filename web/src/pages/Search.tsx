import { useEffect, useRef, useState } from 'react';
import { useNavigate, useSearchParams } from 'react-router-dom';
import { api } from '../api/client';
import type { SearchMedia, StreamEvent } from '../api/types';

function kindOf(m: SearchMedia): 'movie' | 'tv' {
  return m.kind === 'tv' || m.kind === 'anime' ? 'tv' : 'movie';
}

export default function Search() {
  const nav = useNavigate();
  const [params, setParams] = useSearchParams();
  const wd = params.get('wd') || '';
  const [input, setInput] = useState(wd);
  const [media, setMedia] = useState<SearchMedia[]>([]);
  const [streaming, setStreaming] = useState(false);
  const [done, setDone] = useState(false);
  const [sourceInfo, setSourceInfo] = useState('');
  const [err, setErr] = useState('');
  const [filter, setFilter] = useState<'all' | 'movie' | 'tv'>('all');
  const abortRef = useRef<AbortController | null>(null);

  useEffect(() => { setInput(wd); if (wd) run(wd); /* eslint-disable-next-line */ }, [wd]);

  // 每个源结算就增量渲染（同安卓）：首源立即出结果，后续源节流合入，避免频繁重排。
  async function run(keyword: string) {
    abortRef.current?.abort();
    const ac = new AbortController();
    abortRef.current = ac;
    setStreaming(true); setDone(false); setErr(''); setMedia([]); setSourceInfo('');

    let accItems: any[] = [];
    let settled = 0;
    let renderTimer: ReturnType<typeof setTimeout> | null = null;
    let lastRender = 0;

    // 源阶段渲染时按关键词做相关性过滤（后端 done 也会做），避免先渲染一堆不相关再骤减。
    const kw = keyword.trim().toLowerCase();
    const doRender = () => {
      if (renderTimer) { clearTimeout(renderTimer); renderTimer = null; }
      lastRender = Date.now();
      const relevant = accItems.filter((it: any) => {
        const n = String(it.name || '').toLowerCase();
        return kw === '' || n.includes(kw) || kw.includes(n);
      });
      setMedia(api.getAggregate(relevant));
    };
    const renderSoon = (force: boolean) => {
      if (force) { doRender(); return; }
      const gap = Date.now() - lastRender;
      if (gap > 150) { doRender(); }
      else if (!renderTimer) { renderTimer = setTimeout(doRender, 150 - gap); }
    };

    try {
      await api.searchStream(keyword, (e: StreamEvent) => {
        if (ac.signal.aborted) return;
        if (e.type === 'source') {
          settled++;
          if (e.ok && e.list.length) {
            accItems = accItems.concat(e.list);
            renderSoon(!media.length && accItems.length > 0); // 首源立即渲染
          }
          setSourceInfo(`${settled} 个源已返回`);
        } else if (e.type === 'partial_done') {
          if (accItems.length) renderSoon(true);
        } else if (e.type === 'done') {
          if (renderTimer) { clearTimeout(renderTimer); renderTimer = null; }
          const finalList = e.media && e.media.length ? e.media : api.getAggregate(e.list);
          setMedia(finalList);
          setDone(true); setStreaming(false);
        }
      }, ac.signal);
      setDone(true); setStreaming(false);
    } catch (e: any) {
      if (e?.name === 'AbortError') return;
      setErr(e?.message || '搜索失败');
      setStreaming(false); setDone(true);
    } finally {
      if (renderTimer) clearTimeout(renderTimer);
    }
  }

  function submit(e: React.FormEvent) {
    e.preventDefault();
    const kw = input.trim();
    if (!kw) return;
    setParams({ wd: kw });
  }

  const filtered = media.filter(m => {
    if (filter === 'all') return true;
    if (filter === 'movie') return m.kind === 'movie';
    return m.kind === 'tv' || m.kind === 'anime';
  });
  const waitingFirst = streaming && media.length === 0;

  return (
    <div className="page-pad" style={{ paddingTop: 100 }}>
      <form className="search-hero" onSubmit={submit}>
        <input value={input} onChange={e => setInput(e.target.value)} placeholder="搜索影片…" autoFocus />
        <button className="btn play" type="submit" style={{ padding: '0 30px' }}>搜索</button>
      </form>
      <div className="seg">
        <span className={filter === 'all' ? 'active' : ''} onClick={() => setFilter('all')}>全部</span>
        <span className={filter === 'movie' ? 'active' : ''} onClick={() => setFilter('movie')}>电影</span>
        <span className={filter === 'tv' ? 'active' : ''} onClick={() => setFilter('tv')}>剧集与动漫</span>
      </div>

      {waitingFirst ? <div className="search-status"><span className="dotpulse" />搜索中…</div>
        : streaming ? <div className="search-status"><span className="dotpulse" />搜索中 · {sourceInfo} · 已 {filtered.length} 部</div>
          : done && wd ? <div className="search-status">共 {filtered.length} 部作品{sourceInfo ? ` · ${sourceInfo}` : ''}</div> : null}

      {err ? <div className="error-box" style={{ marginBottom: 16 }}>{err}</div> : null}

      <div className="grid" style={{ marginTop: 18 }}>
        {filtered.map(m => (
          <div className="gcard" key={m.id} onClick={() => {
            const src = m.sources[0];
            if (src) nav(`/play/${encodeURIComponent(src.sourceKey)}/${encodeURIComponent(src.vodId)}`);
          }}>
            <div className="poster">
              {(() => {
                const src = m.cover && m.cover.startsWith('/')
                  ? m.cover
                  : api.coverUrl({ title: m.title, year: m.year, kind: kindOf(m), src: m.sources[0]?.pic || m.cover, big: false });
                return <img src={src} alt={m.title} loading="lazy" decoding="async"
                  onError={e => { (e.currentTarget as HTMLImageElement).style.display = 'none'; }} />;
              })()}
              <span className="srcbadge">{m.sourceCount} 源</span>
            </div>
            <div className="n">{m.title}</div>
          </div>
        ))}
      </div>

      {!streaming && done && filtered.length === 0 && !err ? (
        <div className="center-fill"><div className="muted">没有找到「{wd}」的片源</div></div>
      ) : null}
    </div>
  );
}
