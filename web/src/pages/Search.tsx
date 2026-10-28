import { useEffect, useRef, useState } from 'react';
import { useNavigate, useSearchParams } from 'react-router-dom';
import { api } from '../api/client';
import type { SearchMedia, StreamEvent } from '../api/types';
import PosterCard from '../components/PosterCard';

export default function Search() {
  const nav = useNavigate();
  const [params, setParams] = useSearchParams();
  const wd = params.get('wd') || '';
  const [input, setInput] = useState(wd);
  const [media, setMedia] = useState<SearchMedia[]>([]);
  const [loading, setLoading] = useState(false);
  const [done, setDone] = useState(false);
  const [sourceInfo, setSourceInfo] = useState('');
  const [err, setErr] = useState('');
  const [filter, setFilter] = useState<'all' | 'movie' | 'tv'>('all');
  const abortRef = useRef<AbortController | null>(null);

  useEffect(() => { setInput(wd); if (wd) run(wd); /* eslint-disable-next-line */ }, [wd]);

  async function run(keyword: string) {
    abortRef.current?.abort();
    const ac = new AbortController();
    abortRef.current = ac;
    setLoading(true); setDone(false); setErr(''); setMedia([]); setSourceInfo('');

    // 首源即时聚合的累加器（done 事件到达后以后端 media 为准）
    let accItems: any[] = [];
    let settled = 0, total = 0;

    try {
      await api.searchStream(keyword, (e: StreamEvent) => {
        if (ac.signal.aborted) return;
        if (e.type === 'source') {
          settled++;
          if (e.ok && e.list.length) {
            accItems = accItems.concat(e.list);
            setMedia(api.getAggregate(accItems));  // 首源即可见
          }
          setSourceInfo(`已收到 ${settled}${total ? '/' + total : ''} 个源`);
        } else if (e.type === 'partial_done') {
          setLoading(false);
          if (accItems.length) setMedia(api.getAggregate(accItems));
        } else if (e.type === 'done') {
          const finalList = e.media && e.media.length ? e.media : api.getAggregate(e.list);
          setMedia(finalList);
          setDone(true); setLoading(false);
        }
      }, ac.signal);
      setDone(true); setLoading(false);
    } catch (e: any) {
      if (e?.name === 'AbortError') return;
      setErr(e?.message || '搜索失败');
      setLoading(false); setDone(true);
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

      {loading ? <div className="search-status"><span className="dotpulse" />{sourceInfo || '搜索中…'}</div>
        : done && wd ? <div className="search-status">共 {filtered.length} 部作品{sourceInfo ? ` · ${sourceInfo}` : ''}</div> : null}

      {err ? <div className="error-box" style={{ marginBottom: 16 }}>{err}</div> : null}

      <div className="grid" style={{ marginTop: 18 }}>
        {filtered.map(m => (
          <div className="gcard" key={m.id} onClick={() => {
            const src = m.sources[0];
            if (src) nav(`/play/${encodeURIComponent(src.sourceKey)}/${encodeURIComponent(src.vodId)}`);
          }}>
            <div className="poster">
              {m.cover || m.backdrop ? (
                <img src={api.proxyUrl(m.cover || m.backdrop || '')} alt={m.title} loading="lazy" decoding="async"
                  onError={e => { (e.currentTarget as HTMLImageElement).style.display = 'none'; }} />
              ) : null}
              <span className="srcbadge">{m.sourceCount} 源</span>
            </div>
            <div className="n">{m.title}</div>
          </div>
        ))}
      </div>

      {!loading && done && filtered.length === 0 && !err ? (
        <div className="center-fill"><div className="muted">没有找到「{wd}」的片源</div></div>
      ) : null}
    </div>
  );
}
