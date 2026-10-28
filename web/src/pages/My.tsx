import { useEffect, useState } from 'react';
import { useNavigate } from 'react-router-dom';
import { api } from '../api/client';
import type { FavoriteItem, WatchProgress } from '../api/types';

// 「我的」：收藏 + 观看历史（历史可删除）。
export default function My() {
  const nav = useNavigate();
  const [tab, setTab] = useState<'fav' | 'history'>('fav');
  const [favs, setFavs] = useState<FavoriteItem[]>([]);
  const [hist, setHist] = useState<WatchProgress[]>([]);
  const [loading, setLoading] = useState(true);
  const [err, setErr] = useState('');

  async function load() {
    setLoading(true); setErr('');
    try {
      const [f, h] = await Promise.all([
        api.favorites().catch(() => []),
        api.history(false, 200).catch(() => []),
      ]);
      setFavs(f); setHist(h);
    } catch (e: any) { setErr(e?.message || '加载失败'); }
    finally { setLoading(false); }
  }
  useEffect(() => { load(); }, []);

  async function removeFav(id: string) {
    setFavs(prev => prev.filter(x => x.id !== id));
    try { await api.removeFavorite(id); } catch (e: any) { setErr(e?.message || '取消失败'); load(); }
  }
  async function removeHist(id: string) {
    setHist(prev => prev.filter(x => x.id !== id));
    try { await api.deleteHistory(id); } catch (e: any) { setErr(e?.message || '删除失败'); load(); }
  }

  return (
    <div className="page-pad" style={{ paddingTop: 100 }}>
      <div style={{ display: 'flex', alignItems: 'center', gap: 18, marginBottom: 20 }}>
        <div className="page-title" style={{ margin: 0 }}>我的</div>
        <div className="seg" style={{ margin: 0 }}>
          <span className={tab === 'fav' ? 'active' : ''} onClick={() => setTab('fav')}>收藏</span>
          <span className={tab === 'history' ? 'active' : ''} onClick={() => setTab('history')}>历史</span>
        </div>
      </div>

      {err ? <div className="error-box" style={{ marginBottom: 14 }}>{err}</div> : null}
      {loading ? <div className="center-fill"><div className="spinner" /></div> : null}

      {!loading && tab === 'fav' ? (
        <>
          {favs.length === 0 ? <div className="center-fill"><div className="muted">还没有收藏</div></div> : (
            <div className="grid" style={{ marginTop: 4 }}>
              {favs.map(f => (
                <div className="gcard" key={f.id}>
                  <div className="poster" onClick={() => nav(`/play/${encodeURIComponent(f.sourceKey)}/${encodeURIComponent(f.vodId)}`)}>
                    <img src={api.coverUrl({ title: f.title, src: f.cover, big: false })} alt={f.title} loading="lazy" decoding="async"
                      onError={e => { (e.currentTarget as HTMLImageElement).style.display = 'none'; }} />
                    <button className="card-del" title="取消收藏" onClick={(e) => { e.stopPropagation(); removeFav(f.id); }}>✕</button>
                  </div>
                  <div className="n" onClick={() => nav(`/play/${encodeURIComponent(f.sourceKey)}/${encodeURIComponent(f.vodId)}`)}>{f.title}</div>
                </div>
              ))}
            </div>
          )}
        </>
      ) : null}

      {!loading && tab === 'history' ? (
        <>
          {hist.length === 0 ? <div className="center-fill"><div className="muted">暂无观看历史</div></div> : (
            <div style={{ display: 'flex', flexDirection: 'column', gap: 8 }}>
              {hist.map(h => {
                const pct = h.duration > 0 ? Math.min(100, (h.position / h.duration) * 100) : 0;
                return (
                  <div key={h.id} style={{ display: 'flex', alignItems: 'center', gap: 14, background: '#141414', border: '1px solid #262626', borderRadius: 10, padding: 12, cursor: 'pointer' }}
                    onClick={() => nav(`/play/${encodeURIComponent(h.sourceKey)}/${encodeURIComponent(h.vodId)}?ep=${encodeURIComponent(h.episodeId || '')}`)}>
                    <div style={{ width: 84, height: 48, borderRadius: 6, overflow: 'hidden', background: '#0d0d0d', flex: 'none', position: 'relative' }}>
                      <img src={api.coverUrl({ title: h.title || h.vodId, src: h.cover, big: false })} alt="" loading="lazy"
                        style={{ width: '100%', height: '100%', objectFit: 'cover' }}
                        onError={e => { (e.currentTarget as HTMLImageElement).style.display = 'none'; }} />
                      {pct > 0 ? <div style={{ position: 'absolute', left: 0, bottom: 0, height: 3, width: `${pct}%`, background: '#e50914' }} /> : null}
                    </div>
                    <div style={{ flex: 1, minWidth: 0 }}>
                      <div style={{ fontWeight: 600, whiteSpace: 'nowrap', overflow: 'hidden', textOverflow: 'ellipsis' }}>{h.title || h.vodId}</div>
                      <div className="muted" style={{ fontSize: 12 }}>
                        {h.episodeName ? `${h.episodeName} · ` : ''}{h.completed ? '已看完' : `看到 ${fmt(h.position)}${h.duration > 0 ? ' / ' + fmt(h.duration) : ''}`}
                      </div>
                    </div>
                    <button style={{ color: '#ff6b6b', fontSize: 13, padding: '6px 10px' }}
                      onClick={(e) => { e.stopPropagation(); removeHist(h.id); }}>删除</button>
                  </div>
                );
              })}
            </div>
          )}
        </>
      ) : null}
    </div>
  );
}

function fmt(sec: number): string {
  const s = Math.max(0, Math.floor(sec || 0));
  const m = Math.floor(s / 60);
  const r = s % 60;
  return `${m}:${String(r).padStart(2, '0')}`;
}
