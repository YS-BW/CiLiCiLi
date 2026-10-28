import { useEffect, useState } from 'react';
import { useNavigate } from 'react-router-dom';
import { api } from '../api/client';
import type { RecommendItem, WatchProgress } from '../api/types';
import PosterCard from '../components/PosterCard';

export default function Home() {
  const nav = useNavigate();
  const [movies, setMovies] = useState<RecommendItem[]>([]);
  const [tvs, setTvs] = useState<RecommendItem[]>([]);
  const [history, setHistory] = useState<WatchProgress[]>([]);
  const [loading, setLoading] = useState(true);
  const [err, setErr] = useState('');

  useEffect(() => {
    (async () => {
      try {
        const [m, t, h] = await Promise.all([
          api.douban('movie').catch(() => []),
          api.douban('tv').catch(() => []),
          api.history(true, 20).catch(() => []),
        ]);
        setMovies(m); setTvs(t); setHistory(h);
      } catch (e: any) {
        setErr(e?.message || '加载失败');
      } finally {
        setLoading(false);
      }
    })();
  }, []);

  const featured = movies[0] || history[0] || null;
  const heroTitle = featured ? ('title' in featured ? featured.title : '') : '';
  const heroCover = featured ? ('backdrop' in (featured as any) ? (featured as any).backdrop : (featured as any).cover || (featured as any).pic) : '';

  return (
    <div>
      {loading ? (
        <div className="hero-skeleton" />
      ) : featured ? (
        <section className="hero">
          <div className="bg" style={{ backgroundImage: heroCover ? `url(${api.proxyUrl(heroCover)})` : 'radial-gradient(120% 90% at 78% 30%,#3a2a20,#050505)' }} />
          <div className="content">
            <div className="kicker"><b>C</b> 本周推荐</div>
            <h1>{heroTitle}</h1>
            <div className="meta">
              {'score' in featured && (featured as any).score ? <span className="score">★ {(featured as any).score}</span> : null}
              {(featured as any).year ? <span>{(featured as any).year}</span> : null}
              {'type' in featured && (featured as any).type ? <span>{(featured as any).type}</span> : null}
            </div>
            <div className="desc">点击「搜索」按片名查找可用片源与线路。</div>
            <div>
              <button className="btn play" onClick={() => nav(`/search?wd=${encodeURIComponent(heroTitle)}`)}>▶ 查找片源</button>
            </div>
          </div>
        </section>
      ) : null}

      <div className={`rows${featured ? '' : ' flat'}`}>
        {err ? <div className="row"><div className="error-box" style={{ margin: '0 48px' }}>{err}</div></div> : null}

        {history.length > 0 && (
          <div className="row">
            <h3>继续观看</h3>
            <div className="rail">
              {history.map(h => {
                const pct = h.duration > 0 ? (h.position / h.duration) * 100 : 0;
                return (
                  <PosterCard key={h.id} wide title={h.title || h.vodId} cover={h.cover}
                    sub={`${h.episodeName || ''}`} progress={pct}
                    onClick={() => nav(`/play/${encodeURIComponent(h.sourceKey)}/${encodeURIComponent(h.vodId)}?ep=${encodeURIComponent(h.episodeId || '')}`)} />
                );
              })}
            </div>
          </div>
        )}

        {movies.length > 0 && (
          <div className="row">
            <h3>热门电影</h3>
            <div className="rail">
              {movies.map(m => (
                <PosterCard key={m.id} title={m.title} cover={m.cover || m.pic}
                  sub={m.score ? `★ ${m.score}` : undefined}
                  onClick={() => nav(`/search?wd=${encodeURIComponent(m.title)}`)} />
              ))}
            </div>
          </div>
        )}

        {tvs.length > 0 && (
          <div className="row">
            <h3>热门剧集</h3>
            <div className="rail">
              {tvs.map(m => (
                <PosterCard key={m.id} title={m.title} cover={m.cover || m.pic}
                  sub={m.score ? `★ ${m.score}` : undefined}
                  onClick={() => nav(`/search?wd=${encodeURIComponent(m.title)}`)} />
              ))}
            </div>
          </div>
        )}

        {!loading && movies.length === 0 && tvs.length === 0 && history.length === 0 && (
          <div className="center-fill"><div className="muted">暂无推荐内容，试试搜索</div></div>
        )}
      </div>
    </div>
  );
}
