import { useEffect, useState } from 'react';
import { useNavigate } from 'react-router-dom';
import { api } from '../api/client';
import type { RecommendItem, WatchProgress } from '../api/types';
import PosterCard from '../components/PosterCard';

// 模块级缓存：从其它页返回首页时先用缓存秒开，后台再刷新（SWR），避免骨架闪烁/仿佛重新加载。
interface HomeCache {
  movies: RecommendItem[];
  tvs: RecommendItem[];
  history: WatchProgress[];
  heroTitle: string;
  heroKind: 'movie' | 'tv';
  ts: number;
}
let HOME_CACHE: HomeCache | null = null;

export default function Home() {
  const nav = useNavigate();
  const cached = HOME_CACHE;
  const [movies, setMovies] = useState<RecommendItem[]>(cached?.movies || []);
  const [tvs, setTvs] = useState<RecommendItem[]>(cached?.tvs || []);
  const [history, setHistory] = useState<WatchProgress[]>(cached?.history || []);
  const [heroTitleState, setHeroTitleState] = useState(cached?.heroTitle || '');
  const [heroKindState, setHeroKindState] = useState<'movie' | 'tv'>(cached?.heroKind || 'movie');
  const [loading, setLoading] = useState(!cached);   // 有缓存则不再显示骨架
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
        const head = m[0] || t[0];
        const kind: 'movie' | 'tv' = m[0] ? 'movie' : 'tv';
        if (head) { setHeroTitleState(head.title); setHeroKindState(kind); }
        HOME_CACHE = { movies: m, tvs: t, history: h, heroTitle: head?.title || '', heroKind: kind, ts: Date.now() };
      } catch (e: any) {
        setErr(e?.message || '加载失败');
      } finally {
        setLoading(false);
      }
    })();
  }, []);

  const featured = movies[0] || tvs[0] || null;
  const heroTitle = heroTitleState || (featured && 'title' in featured ? (featured as any).title : '');
  const heroRaw = featured ? ('cover' in (featured as any) ? (featured as any).cover : (featured as any).pic) : '';
  // 主推大图统一走 /api/cover big=1（TMDB 横版剧照，src 用豆瓣图兜底）
  const heroBg = heroTitle ? api.coverUrl({ title: heroTitle, src: heroRaw || '', kind: heroKindState, big: true }) : '';
  const heroPending = loading;

  return (
    <div>
      {heroPending ? (
        <div className="hero-skeleton" />
      ) : heroTitle ? (
        <section className="hero wide">
          <div className="bg-wide" style={{ backgroundImage: `url(${heroBg})` }} />
          <div className="hero-fade" />
          <div className="content">
            <div className="kicker"><b>C</b> 本周推荐</div>
            <h1>{heroTitle}</h1>
            <div className="meta">
              {(featured as any)?.score ? <span className="score">★ {(featured as any).score}</span> : null}
              {(featured as any)?.year ? <span>{(featured as any).year}</span> : null}
              {(featured as any)?.type ? <span>{(featured as any).type}</span> : null}
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
                  <PosterCard key={h.id} wide title={h.title || h.vodId} src={h.cover}
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
                <PosterCard key={m.id} title={m.title} kind="movie" src={m.cover || m.pic}
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
                <PosterCard key={m.id} title={m.title} kind="tv" src={m.cover || m.pic}
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
