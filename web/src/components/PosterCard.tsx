import { api } from '../api/client';

interface PosterCardProps {
  title: string;
  cover?: string;
  backdrop?: string;
  badge?: string;       // 源数量等角标
  sub?: string;
  wide?: boolean;       // 16:9（继续观看/同片源）还是 2:3 海报
  progress?: number;    // 0-100
  onClick?: () => void;
}

// 统一的卡片：优先本地封面 / backdrop，加载失败回退，不猜第二张源图。
export default function PosterCard({ title, cover, backdrop, badge, sub, wide, progress, onClick }: PosterCardProps) {
  const src = api.proxyUrl(backdrop || cover || '');
  return (
    <div className={`tile${wide ? '' : ' poster-tile'}`} onClick={onClick}>
      <div className="poster">
        {src ? (
          <img src={src} alt={title} loading="lazy" decoding="async"
               onError={(e) => { (e.currentTarget as HTMLImageElement).style.display = 'none'; }} />
        ) : null}
        {badge ? <span className="srcbadge">{badge}</span> : null}
        {progress != null ? <div className="bar"><i style={{ width: `${Math.min(100, progress)}%` }} /></div> : null}
      </div>
      <div className="name">{title}</div>
      {sub ? <div className="sub">{sub}</div> : null}
    </div>
  );
}
