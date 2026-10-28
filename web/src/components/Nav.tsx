import { useEffect, useState } from 'react';
import { NavLink, useLocation, useNavigate } from 'react-router-dom';
import { useAuth } from '../lib/auth';
import { getSearchMode, setSearchMode, type SearchMode } from '../lib/settings';

export default function Nav() {
  const [solid, setSolid] = useState(false);
  const [q, setQ] = useState('');
  const [showSettings, setShowSettings] = useState(false);
  const [mode, setMode] = useState<SearchMode>(getSearchMode());
  const nav = useNavigate();
  const loc = useLocation();
  const { logout } = useAuth();

  useEffect(() => {
    const onScroll = () => setSolid(window.scrollY > 40);
    window.addEventListener('scroll', onScroll);
    return () => window.removeEventListener('scroll', onScroll);
  }, []);

  // 在搜索结果页时，输入框回填当前关键词
  useEffect(() => {
    const p = new URLSearchParams(loc.search);
    setQ(p.get('wd') || '');
  }, [loc.search]);

  function submit(e: React.FormEvent) {
    e.preventDefault();
    const kw = q.trim();
    if (kw) nav(`/search?wd=${encodeURIComponent(kw)}`);
  }

  function chooseMode(m: SearchMode) { setMode(m); setSearchMode(m); }

  return (
    <nav className={`nav${solid ? ' solid' : ''}`}>
      <div className="brand">CILICILI</div>
      <div className="links">
        <NavLink to="/" end className={({ isActive }) => (isActive ? 'active' : '')}>首页</NavLink>
        <NavLink to="/me" className={({ isActive }) => (isActive ? 'active' : '')}>我的</NavLink>
        <NavLink to="/sources" className={({ isActive }) => (isActive ? 'active' : '')}>源管理</NavLink>
      </div>

      <div className="nav-search-wrap">
        <form className="nav-search" onSubmit={submit}>
          <span className="nav-search-icon">🔍</span>
          <input value={q} onChange={e => setQ(e.target.value)} placeholder="搜索影片…" />
        </form>
      </div>

      <div className="nav-right">
        <button className="icon-btn" title="设置" onClick={() => setShowSettings(v => !v)}>⚙</button>
        {showSettings ? (
          <div className="settings-panel">
            <div className="sp-title">设置</div>
            <div className="sp-section">搜索模式</div>
            <label className="sp-radio">
              <input type="radio" checked={mode === 'fast'} onChange={() => chooseMode('fast')} />
              <span>快速搜索<em>只查最快的若干源，更快</em></span>
            </label>
            <label className="sp-radio">
              <input type="radio" checked={mode === 'full'} onChange={() => chooseMode('full')} />
              <span>完整搜索<em>查全部源，更全但更慢</em></span>
            </label>
            <div className="sp-divider" />
            <button className="sp-logout" onClick={() => logout()}>退出登录</button>
          </div>
        ) : null}
      </div>
    </nav>
  );
}
