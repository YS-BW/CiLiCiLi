import { createContext, useContext, useEffect, useState, type ReactNode } from 'react';
import { api, ApiError } from '../api/client';

interface AuthState {
  ready: boolean;
  verified: boolean;
  passwordRequired: boolean;
  version: string;
  login: (password: string) => Promise<void>;
  logout: () => Promise<void>;
  refresh: () => Promise<void>;
}

const Ctx = createContext<AuthState | null>(null);

export function AuthProvider({ children }: { children: ReactNode }) {
  const [ready, setReady] = useState(false);
  const [verified, setVerified] = useState(false);
  const [passwordRequired, setPasswordRequired] = useState(true);
  const [version, setVersion] = useState('');

  async function refresh() {
    try {
      const st = await api.status();
      setPasswordRequired(st.passwordRequired);
      setVersion(st.version);
      if (!st.passwordRequired) { setVerified(true); return; }
      try {
        const a = await api.authState();
        setVerified(!!a.verified);
      } catch (e) {
        if (e instanceof ApiError && e.status === 401) setVerified(false);
        else setVerified(false);
      }
    } catch {
      setVerified(false);
    }
  }

  useEffect(() => {
    refresh().finally(() => setReady(true));
  }, []);

  async function login(password: string) {
    const r = await api.login(password);
    if (!r.success) throw new Error(r.error || '登录失败');
    setVerified(true);
  }
  async function logout() {
    try { await api.logout(); } catch { /* ignore */ }
    setVerified(false);
  }

  return (
    <Ctx.Provider value={{ ready, verified, passwordRequired, version, login, logout, refresh }}>
      {children}
    </Ctx.Provider>
  );
}

export function useAuth(): AuthState {
  const v = useContext(Ctx);
  if (!v) throw new Error('useAuth outside AuthProvider');
  return v;
}
