import { useEffect } from 'react';
import { api } from '../api/client';
import { prefetchImage } from '../lib/prefetch';

interface PosterCardProps {
  title: string;
  year?: string;
  kind?: 'movie' | 'tv';
  src?: string;         // 已知原始图（采集源 pic / 豆瓣图），交给 /api/cover 并发兜底
  cover?: string;       // 兼容旧用法：站内 /api/media 封面直接使用
  badge?: string;
  sub?: string;
  wide?: boolean;       // 16:9（继续观看）还是 2:3 海报
  progress?: number;
  onClick?: () => void;
}

// 统一卡片：所有封面经 /api/cover（后端 TMDB+源并发、缓存）。
export default function PosterCard({ title, year, kind, src, cover, badge, sub, wide, progress, onClick }: PosterCardProps) {
  const imgSrc = src && src.startsWith('/')
    ? src
    : (cover && cover.startsWith('/') && !src ? cover : api.coverUrl({ title, year, kind, src: src || cover, big: false }));

  // 小封面渲染时，后台预加载该片大封面（big=1），进详情秒显示。
  useEffect(() => {
    if (!title) return;
    prefetchImage(api.coverUrl({ title, year, kind, src: src || cover, big: true }));
  }, [title, year, kind, src, cover]);

  return (
    <div className={`tile${wide ? '' : ' poster-tile'}`} onClick={onClick}>
      <div className="poster">
        <img src={imgSrc} alt={title} loading="lazy" decoding="async"
             onError={(e) => { (e.currentTarget as HTMLImageElement).style.display = 'none'; }} />
        {badge ? <span className="srcbadge">{badge}</span> : null}
        {progress != null ? <div className="bar"><i style={{ width: `${Math.min(100, progress)}%` }} /></div> : null}
      </div>
      <div className="name">{title}</div>
      {sub ? <div className="sub">{sub}</div> : null}
    </div>
  );
}
