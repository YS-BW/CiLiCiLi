import { useEffect, useState } from 'react';
import { useNavigate } from 'react-router-dom';
import { api } from '../api/client';
import type { FavoriteItem } from '../api/types';

export default function Favorites() {
  const nav = useNavigate();
  const [items, setItems] = useState<FavoriteItem[]>([]);
  const [loading, setLoading] = useState(true);
  const [err, setErr] = useState('');

  useEffect(() => {
    (async () => {
      try {
        setItems(await api.favorites());
      } catch (e: any) {
        setErr(e?.message || '加载失败');
      } finally {
        setLoading(false);
      }
    })();
  }, []);

  return (
    <div className="page-pad" style={{ paddingTop: 100 }}>
      <div className="page-title">我的收藏</div>
      {loading ? <div className="center-fill"><div className="spinner" /></div> : null}
      {err ? <div className="error-box">{err}</div> : null}
      {!loading && !err && items.length === 0 ? (
        <div className="center-fill"><div className="muted">还没有收藏</div></div>
      ) : null}
      <div className="grid" style={{ marginTop: 18 }}>
        {items.map(f => (
          <div className="gcard" key={f.id} onClick={() => nav(`/play/${encodeURIComponent(f.sourceKey)}/${encodeURIComponent(f.vodId)}`)}>
            <div className="poster">
              <img src={api.coverUrl({ title: f.title, src: f.cover, big: false })} alt={f.title} loading="lazy" decoding="async"
                onError={e => { (e.currentTarget as HTMLImageElement).style.display = 'none'; }} />
            </div>
            <div className="n">{f.title}</div>
          </div>
        ))}
      </div>
    </div>
  );
}
