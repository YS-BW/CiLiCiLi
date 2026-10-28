import { useEffect, useState } from 'react';
import { NavLink, useNavigate } from 'react-router-dom';
import { useAuth } from '../lib/auth';

export default function Nav() {
  const [solid, setSolid] = useState(false);
  const [q, setQ] = useState('');
  const nav = useNavigate();
  const { logout } = useAuth();

  useEffect(() => {
    const onScroll = () => setSolid(window.scrollY > 40);
    window.addEventListener('scroll', onScroll);
    return () => window.removeEventListener('scroll', onScroll);
  }, []);

  function submit(e: React.FormEvent) {
    e.preventDefault();
    const kw = q.trim();
    if (kw) nav(`/search?wd=${encodeURIComponent(kw)}`);
  }

  return (
    <nav className={`nav${solid ? ' solid' : ''}`}>
      <div className="brand">CILICILI</div>
      <div className="links">
        <NavLink to="/" end className={({ isActive }) => (isActive ? 'active' : '')}>首页</NavLink>
        <NavLink to="/search" className={({ isActive }) => (isActive ? 'active' : '')}>搜索</NavLink>
        <NavLink to="/favorites" className={({ isActive }) => (isActive ? 'active' : '')}>我的收藏</NavLink>
      </div>
      <div className="spacer" />
      <form onSubmit={submit} style={{ display: 'flex', alignItems: 'center' }}>
        <input
          value={q}
          onChange={e => setQ(e.target.value)}
          placeholder="搜索影片…"
          style={{ background: 'rgba(0,0,0,.6)', border: '1px solid #333', borderRadius: 6, padding: '8px 12px', color: '#fff', width: 200, outline: 'none' }}
        />
      </form>
      <button className="icon-btn" title="退出登录" onClick={() => logout()}>⏻</button>
    </nav>
  );
}
