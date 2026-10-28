import { useState } from 'react';
import { useLocation, useNavigate } from 'react-router-dom';
import { useAuth } from '../lib/auth';

export default function Login() {
  const { login } = useAuth();
  const [pw, setPw] = useState('');
  const [err, setErr] = useState('');
  const [busy, setBusy] = useState(false);
  const nav = useNavigate();
  const loc = useLocation() as { state?: { from?: string } };

  async function submit(e: React.FormEvent) {
    e.preventDefault();
    setErr(''); setBusy(true);
    try {
      await login(pw);
      nav(loc.state?.from || '/', { replace: true });
    } catch (e: any) {
      setErr(e?.message || '登录失败');
    } finally {
      setBusy(false);
    }
  }

  return (
    <div className="login">
      <form className="card" onSubmit={submit}>
        <span className="brand">CILICILI</span>
        <p>多源聚合影视 · 自建使用</p>
        <input
          type="password"
          placeholder="服务器访问密码"
          value={pw}
          autoFocus
          onChange={e => setPw(e.target.value)}
        />
        {err ? <div className="error-box" style={{ marginBottom: 14 }}>{err}</div> : null}
        <button className="btn" type="submit" disabled={busy || !pw}>
          {busy ? '登录中…' : '登录'}
        </button>
        <div className="hint">会话存于 httpOnly Cookie</div>
      </form>
    </div>
  );
}
