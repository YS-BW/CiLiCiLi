import { useEffect, useRef, useState } from 'react';
import { NavLink, useLocation, useNavigate } from 'react-router-dom';
import { useAuth } from '../lib/auth';
import { getSearchMode, setSearchMode, type SearchMode } from '../lib/settings';
import { androidBridge, siteOrigin } from '../lib/native';
import { IconSearch, IconGear, IconHome, IconUser, IconFolder } from './icons';

export default function Nav() {
  const [solid, setSolid] = useState(false);
  const [hidden, setHidden] = useState(false);
  const [q, setQ] = useState('');
  const [showSettings, setShowSettings] = useState(false);
  const [mode, setMode] = useState<SearchMode>(getSearchMode());
  const lastY = useRef(0);
  const nav = useNavigate();
  const loc = useLocation();
  const { logout } = useAuth();
  const bridge = androidBridge();

  useEffect(() => {
    const onScroll = () => {
      const y = window.scrollY;
      setSolid(y > 40);
      // 下滑隐藏顶栏、上滑显示
      if (y > lastY.current + 4 && y > 80) { setHidden(true); setShowSettings(false); }
      else if (y < lastY.current - 4) setHidden(false);
      lastY.current = y;
    };
    window.addEventListener('scroll', onScroll, { passive: true });
    return () => window.removeEventListener('scroll', onScroll);
  }, []);

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
    <>
      <nav className={`nav${solid ? ' solid' : ''}${hidden ? ' nav-hidden' : ''}`}>
        <div className="brand">CILICILI</div>
        <div className="links">
          <NavLink to="/" end className={({ isActive }) => (isActive ? 'active' : '')}>首页</NavLink>
          <NavLink to="/me" className={({ isActive }) => (isActive ? 'active' : '')}>我的</NavLink>
          <NavLink to="/sources" className={({ isActive }) => (isActive ? 'active' : '')}>源管理</NavLink>
        </div>

        <div className="nav-search-wrap">
          <form className="nav-search" onSubmit={submit}>
            <IconSearch size={16} className="nav-search-icon" />
            <input value={q} onChange={e => setQ(e.target.value)} placeholder="搜索影片…" />
          </form>
        </div>

        <div className="nav-right">
          <button className="icon-btn" title="设置" onClick={() => setShowSettings(v => !v)}><IconGear size={20} /></button>
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
              {bridge ? (
                <>
                  <div className="sp-section">服务器</div>
                  <div className="sp-server">
                    <span className="sp-server-url" title={siteOrigin()}>{siteOrigin()}</span>
                    <button className="sp-server-btn" onClick={() => bridge.openServerSettings()}>修改</button>
                  </div>
                  <div className="sp-divider" />
                </>
              ) : null}
              <button className="sp-logout" onClick={() => logout()}>退出登录</button>
            </div>
          ) : null}
        </div>
      </nav>

      {/* 移动端底部 Tab 栏 */}
      <nav className="tabbar">
        <NavLink to="/" end className={({ isActive }) => (isActive ? 'active' : '')}><IconHome size={21} /><span>首页</span></NavLink>
        <NavLink to="/me" className={({ isActive }) => (isActive ? 'active' : '')}><IconUser size={21} /><span>我的</span></NavLink>
        <NavLink to="/sources" className={({ isActive }) => (isActive ? 'active' : '')}><IconFolder size={21} /><span>源管理</span></NavLink>
      </nav>
    </>
  );
}
