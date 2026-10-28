import { useEffect, useState } from 'react';
import { api } from '../api/client';
import type { SourceAdminItem, SourceHealth } from '../api/types';

// 源管理：增加、删除、启用/禁用（对接后端 /api/sources）。
export default function Sources() {
  const [items, setItems] = useState<SourceAdminItem[]>([]);
  const [health, setHealth] = useState<Record<string, SourceHealth>>({});
  const [loading, setLoading] = useState(true);
  const [err, setErr] = useState('');
  const [name, setName] = useState('');
  const [url, setUrl] = useState('');
  const [busy, setBusy] = useState(false);

  async function load() {
    setLoading(true); setErr('');
    try {
      const r = await api.sourcesAdmin();
      setItems(r.sources || []);
      const hm: Record<string, SourceHealth> = {};
      for (const h of r.health || []) hm[h.key] = h;
      setHealth(hm);
    } catch (e: any) {
      setErr(e?.message || '加载失败');
    } finally {
      setLoading(false);
    }
  }
  useEffect(() => { load(); }, []);

  async function toggle(s: SourceAdminItem) {
    const next = !s.enabled;
    setItems(prev => prev.map(x => x.key === s.key ? { ...x, enabled: next } : x));
    try { await api.updateSource(s.key, { enabled: next }); }
    catch (e: any) { setErr(e?.message || '更新失败'); load(); }
  }

  async function del(s: SourceAdminItem) {
    if (!confirm(`删除源「${s.name}」？`)) return;
    setBusy(true);
    try { await api.deleteSource(s.key); await load(); }
    catch (e: any) { setErr(e?.message || '删除失败'); }
    finally { setBusy(false); }
  }

  async function resetAll() {
    setBusy(true); setErr('');
    try { await Promise.all(items.map(s => api.resetSource(s.key).catch(() => {}))); await load(); }
    catch (e: any) { setErr(e?.message || '重置失败'); }
    finally { setBusy(false); }
  }

  async function add(e: React.FormEvent) {
    e.preventDefault();
    if (!name.trim() || !url.trim()) return;
    setBusy(true); setErr('');
    try {
      await api.createSource(name.trim(), url.trim());
      setName(''); setUrl(''); await load();
    } catch (e: any) { setErr(e?.message || '添加失败'); }
    finally { setBusy(false); }
  }

  return (
    <div className="page-pad" style={{ paddingTop: 100 }}>
      <div style={{ display: 'flex', alignItems: 'center', gap: 14, marginBottom: 18 }}>
        <div className="page-title" style={{ margin: 0 }}>源管理</div>
        <button className="btn ghost" onClick={resetAll} disabled={busy} style={{ padding: '8px 16px', fontSize: 13 }}>重置全部熔断</button>
      </div>

      <form onSubmit={add} style={{ display: 'flex', gap: 10, marginBottom: 20, flexWrap: 'wrap' }}>
        <input value={name} onChange={e => setName(e.target.value)} placeholder="源名称"
          style={{ background: '#141414', border: '1px solid #2a2a2a', borderRadius: 8, padding: '10px 14px', color: '#fff', width: 180 }} />
        <input value={url} onChange={e => setUrl(e.target.value)} placeholder="采集接口 URL（http/https）"
          style={{ background: '#141414', border: '1px solid #2a2a2a', borderRadius: 8, padding: '10px 14px', color: '#fff', flex: 1, minWidth: 320 }} />
        <button className="btn play" type="submit" disabled={busy} style={{ padding: '10px 22px' }}>添加源</button>
      </form>

      {err ? <div className="error-box" style={{ marginBottom: 14 }}>{err}</div> : null}
      {loading ? <div className="center-fill"><div className="spinner" /></div> : null}

      <div style={{ display: 'flex', flexDirection: 'column', gap: 8 }}>
        {items.map(s => {
          const h = health[s.key];
          const open = h?.openUntil ? '熔断中' : '';
          return (
            <div key={s.key} className="src-row">
              <div className="src-main">
                <div className="src-name">
                  {s.name}
                  <span className="k">{s.key}</span>
                  {open ? <span style={{ color: '#e5a50a', fontSize: 12, marginLeft: 8 }}>{open}</span> : null}
                  {!s.enabled ? <span style={{ color: '#888', fontSize: 12, marginLeft: 8 }}>已禁用</span> : null}
                </div>
                <div className="src-url">{s.url}</div>
              </div>
              <span className="src-lat">{h?.ewmaMs ? `${Math.round(h.ewmaMs)}ms` : ''}</span>
              <div className="src-actions">
                <button className="btn ghost" onClick={() => toggle(s)}>{s.enabled ? '禁用' : '启用'}</button>
                <button className="del" onClick={() => del(s)} disabled={busy}>删除</button>
              </div>
            </div>
          );
        })}
      </div>
      {!loading && items.length === 0 && !err ? <div className="center-fill"><div className="muted">还没有源</div></div> : null}
    </div>
  );
}
