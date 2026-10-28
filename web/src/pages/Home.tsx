import { useEffect, useState } from 'react';
import { useNavigate } from 'react-router-dom';
import { api } from '../api/client';
import type { SearchMedia, WatchProgress } from '../api/types';
import PosterCard from '../components/PosterCard';
import { rememberSources } from '../lib/sourcesCache';

// 模块级缓存：从其它页返回首页时先用缓存秒开，后台再刷新（SWR），避免骨架闪烁。
interface HomeCache {
  movies: SearchMedia[];
  tvs: SearchMedia[];
  history: WatchProgress[];
  ts: number;
}
let HOME_CACHE: HomeCache | null = null;

export default function Home() {
  const nav = useNavigate();
  const cached = HOME_CACHE;
  const [movies, setMovies] = useState<SearchMedia[]>(cached?.movies || []);
  const [tvs, setTvs] = useState<SearchMedia[]>(cached?.tvs || []);
  const [history, setHistory] = useState<WatchProgress[]>(cached?.history || []);
  const [loading, setLoading] = useState(!cached);
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
        HOME_CACHE = { movies: m, tvs: t, history: h, ts: Date.now() };
      } catch (e: any) {
        setErr(e?.message || '加载失败');
      } finally {
        setLoading(false);
      }
    })();
  }, []);

  const featured = movies[0] || tvs[0] || null;
  const heroTitle = featured?.title || '';
  // 主推大图：TMDB 横版剧照；src 用卡片封面兜底（后端 big 只返回 TMDB 横图）
  const heroBg = heroTitle
    ? api.coverUrl({ title: heroTitle, year: featured?.year, kind: kindOf(featured), src: featured?.cover || firstPic(featured), big: true })
    : '';

  // 点卡片：直接用卡片自带的 sources 进详情（不再跳搜索）。
  function openCard(m: SearchMedia) {
    const s = m.sources?.[0];
    if (s && s.sourceKey && s.vodId) {
      rememberSources(m.title, m.sources);
      nav(`/play/${encodeURIComponent(s.sourceKey)}/${encodeURIComponent(s.vodId)}`);
    } else {
      nav(`/search?wd=${encodeURIComponent(m.title)}`);
    }
  }

  return (
    <div>
      {loading ? (
        <div className="hero-skeleton" />
      ) : heroTitle ? (
        <section className="hero wide">
          <div className="bg-wide" style={{ backgroundImage: `url(${heroBg})` }} />
          <div className="hero-fade" />
          <div className="content">
            <div className="kicker"><b>C</b> 本周推荐</div>
            <h1>{heroTitle}</h1>
            <div className="meta">
              {featured?.year ? <span>{featured.year}</span> : null}
              {featured?.typeName ? <span>{featured.typeName}</span> : null}
              {featured && featured.sourceCount > 0 ? <span>{featured.sourceCount} 个片源</span> : null}
            </div>
            <div className="desc">点击下方卡片直接进入影片详情与播放。</div>
            <div>
              <button className="btn play" onClick={() => openCard(featured!)}>▶ 播放</button>
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
                <PosterCard key={m.id} title={m.title} year={m.year} kind={kindOf(m)}
                  src={m.cover || m.sources?.[0]?.pic}
                  sub={cardSub(m)}
                  onClick={() => openCard(m)} />
              ))}
            </div>
          </div>
        )}

        {tvs.length > 0 && (
          <div className="row">
            <h3>热门剧集</h3>
            <div className="rail">
              {tvs.map(m => (
                <PosterCard key={m.id} title={m.title} year={m.year} kind={kindOf(m)}
                  src={m.cover || m.sources?.[0]?.pic}
                  sub={cardSub(m)}
                  onClick={() => openCard(m)} />
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

function cardSub(m: SearchMedia): string | undefined {
  const r = m.rating ? `★ ${m.rating}` : '';
  const sc = m.sourceCount > 0 ? `${m.sourceCount} 源` : '';
  return [r, sc].filter(Boolean).join(' · ') || undefined;
}
function kindOf(m?: SearchMedia | null): 'movie' | 'tv' {
  return m && (m.kind === 'tv' || m.kind === 'anime') ? 'tv' : 'movie';
}
function firstPic(m?: SearchMedia | null): string {
  return m?.sources?.[0]?.pic || '';
}
