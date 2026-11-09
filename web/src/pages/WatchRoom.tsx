import { lazy, Suspense, useEffect, useMemo, useRef, useState } from 'react';
import { useLocation, useNavigate, useParams } from 'react-router-dom';
import type Artplayer from 'artplayer';
import { api } from '../api/client';
import type { SourceRef } from '../api/types';
import { lookupSources } from '../lib/sourceMatch';
import {
  WatchPartyClient,
  WatchPartyError,
  clearMemberCredentials,
  createDriftCorrector,
  joinRoom,
  leaveRoom,
  loadMemberCredentials,
  loadNickname,
  saveNickname,
  type StoredMemberCredentials,
  type WatchDanmaku,
  type WatchMember,
  type WatchState,
} from '../lib/watchparty';

const Player = lazy(() => import('../components/Player'));

interface RoomEpisode {
  id: string;
  name: string;
  index: number;
  url: string;
}

export interface WatchRoomMeta {
  title?: string;
  url?: string;
  episode?: number;
  episodes?: RoomEpisode[];
  detailPath?: string;
  sourceKey?: string;
  vodId?: string;
  sourceName?: string;
}

const META_PREFIX = 'watch:meta:';
const DANMAKU_MAX_LENGTH = 80;
const DANMAKU_COLORS = ['#ffffff', '#ffcc00', '#ff6b6b', '#4ecdc4', '#a78bfa'];

interface DanmakuPlugin {
  emit: (danmu: {
    text: string;
    color?: string;
    mode?: number;
    id?: string;
    time?: number;
  }) => unknown;
}

function getDanmakuPlugin(art: Artplayer | null): DanmakuPlugin | null {
  const plugins = art?.plugins as Record<string, unknown> | undefined;
  const plugin = plugins?.artplayerPluginDanmuku as DanmakuPlugin | undefined;
  return plugin && typeof plugin.emit === 'function' ? plugin : null;
}

function emitDanmaku(art: Artplayer | null, danmaku: WatchDanmaku): boolean {
  const plugin = getDanmakuPlugin(art);
  if (!plugin) return false;
  plugin.emit({
    text: danmaku.text,
    color: danmaku.color || undefined,
    mode: danmaku.mode ?? 0,
    id: danmaku.id,
    time: art?.currentTime,
  });
  return true;
}

function dedupeSources(list: SourceRef[]): SourceRef[] {
  const seen = new Set<string>();
  const out: SourceRef[] = [];
  for (const source of list) {
    if (seen.has(source.sourceKey)) continue;
    seen.add(source.sourceKey);
    out.push(source);
  }
  return out;
}

export function saveWatchRoomMeta(roomId: string, meta: WatchRoomMeta): void {
  try { sessionStorage.setItem(META_PREFIX + roomId, JSON.stringify(meta)); } catch { /* ignore */ }
}

function loadRoomMeta(roomId: string, state: unknown): WatchRoomMeta | null {
  if (state && typeof state === 'object') return state as WatchRoomMeta;
  try {
    const raw = sessionStorage.getItem(META_PREFIX + roomId);
    if (!raw) return null;
    return JSON.parse(raw) as WatchRoomMeta;
  } catch {
    return null;
  }
}

export default function WatchRoom() {
  const { roomId = '' } = useParams();
  const loc = useLocation();
  const navigate = useNavigate();
  const meta = useMemo(() => loadRoomMeta(roomId, loc.state), [roomId, loc.state]);

  const [credentials, setCredentials] = useState<StoredMemberCredentials | null>(() => loadMemberCredentials(roomId));
  const [name, setName] = useState(() => loadNickname());
  const [joining, setJoining] = useState(false);
  const [joinError, setJoinError] = useState('');

  const [state, setState] = useState<WatchState | null>(null);
  const [members, setMembers] = useState<WatchMember[]>([]);
  const [connected, setConnected] = useState(false);
  const [error, setError] = useState('');
  const [copied, setCopied] = useState(false);
  const [art, setArt] = useState<Artplayer | null>(null);
  const [closedNotice, setClosedNotice] = useState('');
  const [roomGone, setRoomGone] = useState(false);
  const [sources, setSources] = useState<SourceRef[]>([]);
  const [sourcesLoading, setSourcesLoading] = useState(false);
  const [switchingSource, setSwitchingSource] = useState('');
  const [sourceError, setSourceError] = useState('');
  const [danmakuText, setDanmakuText] = useState('');
  const [danmakuColor, setDanmakuColor] = useState(DANMAKU_COLORS[0]);

  const clientRef = useRef<WatchPartyClient | null>(null);
  const initialEpisodeSent = useRef(false);
  const sourcesRef = useRef<SourceRef[]>([]);
  const artRef = useRef<Artplayer | null>(null);
  const pendingDanmakuRef = useRef<WatchDanmaku[]>([]);

  useEffect(() => { sourcesRef.current = sources; }, [sources]);

  useEffect(() => {
    artRef.current = art;
    if (!art || !getDanmakuPlugin(art) || pendingDanmakuRef.current.length === 0) return;
    const pending = pendingDanmakuRef.current;
    pendingDanmakuRef.current = [];
    for (const danmaku of pending) emitDanmaku(art, danmaku);
  }, [art]);

  const isHost = useMemo(
    () => !!credentials && members.some(m => m.id === credentials.memberId && m.host),
    [credentials, members],
  );

  const roomTitle = state?.title || meta?.title || '';
  const sourceKey = state?.sourceKey || meta?.sourceKey || '';
  const vodId = state?.vodId || meta?.vodId || '';
  const sourceName = state?.sourceName || meta?.sourceName || '';
  const detailPath = sourceKey && vodId
    ? `/play/${encodeURIComponent(sourceKey)}/${encodeURIComponent(vodId)}`
    : (meta?.detailPath || '/');
  const episodes = useMemo<RoomEpisode[]>(() => {
    const roomEpisodes = state?.episodes;
    if (roomEpisodes && roomEpisodes.length > 0) {
      return roomEpisodes.map((episode, i) => ({ id: `state-${i}`, name: episode.name, index: i, url: episode.url }));
    }
    return meta?.episodes ?? [];
  }, [state?.episodes, meta?.episodes]);
  const currentEpisode = state?.episode ?? meta?.episode ?? 0;

  useEffect(() => {
    if (!credentials) return;
    const client = new WatchPartyClient(roomId, credentials);
    clientRef.current = client;
    const unsubs = [
      client.on('state', s => setState(s)),
      client.on('members', list => setMembers(list)),
      client.on('danmaku', danmaku => {
        if (emitDanmaku(artRef.current, danmaku)) return;
        pendingDanmakuRef.current.push(danmaku);
        if (pendingDanmakuRef.current.length > 50) pendingDanmakuRef.current.shift();
      }),
      client.on('open', () => { setConnected(true); setError(''); }),
      client.on('error', msg => setError(msg)),
      client.on('close', info => {
        setConnected(false);
        if (info.willReconnect) setError('连接中断，正在重连…');
        else setError('连接已断开');
      }),
      client.on('closed', () => {
        setConnected(false);
        setClosedNotice('房主已结束一起看');
      }),
    ];
    client.connect();
    return () => {
      for (const off of unsubs) off();
      client.disconnect();
      if (clientRef.current === client) clientRef.current = null;
    };
  }, [roomId, credentials]);

  useEffect(() => {
    if (!closedNotice) return;
    const timer = window.setTimeout(() => {
      navigate(detailPath);
    }, 2000);
    return () => window.clearTimeout(timer);
  }, [closedNotice, detailPath, navigate]);

  // 从详情页进入时，房主把当前集同步为房间内容。
  useEffect(() => {
    if (!isHost || !meta?.url || state?.url === meta.url || initialEpisodeSent.current) return;
    const client = clientRef.current;
    if (!client) return;
    client.sendCommand({
      op: 'episode',
      episode: meta.episode ?? state?.episode ?? 0,
      url: meta.url,
      positionMs: 0,
      title: meta.title,
      sourceKey: meta.sourceKey,
      vodId: meta.vodId,
      sourceName: meta.sourceName,
      episodes: (meta.episodes ?? []).map(episode => ({ name: episode.name, url: episode.url })),
    });
    initialEpisodeSent.current = true;
  }, [isHost, meta, state?.url, state?.episode]);

  // 房主查找同片其它采集源，供房间内换源；观众只看当前片源。
  useEffect(() => {
    if (!isHost || !roomTitle || !sourceKey) {
      setSources([]);
      setSourcesLoading(false);
      return;
    }

    const current: SourceRef = { sourceKey, vodId, sourceName, name: roomTitle };
    // 切换到已在列表里的片源时无需重新查找。
    if (sourcesRef.current.some(s => s.sourceKey === sourceKey && s.vodId === vodId)) {
      setSourcesLoading(false);
      return;
    }

    let cancelled = false;
    setSourcesLoading(true);
    lookupSources({
      title: roomTitle,
      // 房间内的年份未知，跳过年份校验。
      current,
      search: keyword => api.search(keyword, { fast: true }),
      onUpdate: list => { if (!cancelled) setSources(dedupeSources(list)); },
    })
      .then(list => { if (!cancelled) setSources(dedupeSources(list)); })
      .catch(() => { if (!cancelled) setSources(dedupeSources([current])); })
      .finally(() => { if (!cancelled) setSourcesLoading(false); });
    return () => { cancelled = true; };
  }, [isHost, roomTitle, sourceKey, vodId, sourceName]);

  // 房主的手动播放/暂停/拖动会广播给服务器。
  useEffect(() => {
    if (!isHost || !art) return;
    const client = clientRef.current;
    if (!client) return;
    const positionMs = () => Math.round(art.currentTime * 1000);
    const onPlay = () => client.sendCommand({ op: 'play', positionMs: positionMs() });
    const onPause = () => client.sendCommand({ op: 'pause', positionMs: positionMs() });
    const onSeeked = () => client.sendCommand({ op: 'seek', positionMs: positionMs() });
    art.on('video:play', onPlay);
    art.on('video:pause', onPause);
    art.on('video:seeked', onSeeked);
    return () => {
      art.off('video:play', onPlay);
      art.off('video:pause', onPause);
      art.off('video:seeked', onSeeked);
    };
  }, [isHost, art]);

  // 观众：每 500ms 用服务端权威时间轴纠正本地播放，手动控制会被拉回。
  useEffect(() => {
    if (isHost || !art) return;
    const client = clientRef.current;
    if (!client) return;
    const corrector = createDriftCorrector({
      get currentTime() { return art.currentTime; },
      set currentTime(value: number) { art.currentTime = value; },
      get playbackRate() { return art.playbackRate; },
      set playbackRate(value: number) { art.playbackRate = value as Artplayer['playbackRate']; },
      get paused() { return art.video.paused; },
      play: () => art.play(),
      pause: () => art.pause(),
    }, client, { intervalMs: 500 });
    corrector.start();
    return () => corrector.stop();
  }, [isHost, art]);

  useEffect(() => {
    if (!roomTitle) return;
    const prev = document.title;
    document.title = `${roomTitle} · 一起看`;
    return () => { document.title = prev; };
  }, [roomTitle]);

  async function submitJoin(e: React.FormEvent) {
    e.preventDefault();
    const displayName = name.trim();
    if (!displayName || joining) return;
    setJoining(true);
    setJoinError('');
    try {
      const joined = await joinRoom(roomId, displayName);
      saveNickname(displayName);
      setState(joined.state);
      setCredentials({ memberId: joined.memberId, memberToken: joined.memberToken, name: joined.name });
    } catch (err) {
      if (err instanceof WatchPartyError && err.status === 404) {
        setRoomGone(true);
        return;
      }
      setJoinError(err instanceof Error ? err.message : '加入房间失败');
    } finally {
      setJoining(false);
    }
  }

  async function handleLeave() {
    if (isHost && !window.confirm('退出将解散房间，所有人都会退出，确定吗？')) return;
    try {
      await leaveRoom(roomId);
    } catch {
      // 房间可能已不存在，仍然允许本地退出。
    }
    clientRef.current?.disconnect();
    clientRef.current = null;
    navigate(detailPath);
  }

  function changeIdentity() {
    clearMemberCredentials(roomId);
    clientRef.current?.disconnect();
    clientRef.current = null;
    initialEpisodeSent.current = false;
    setCredentials(null);
    setState(null);
    setMembers([]);
    setConnected(false);
    setError('');
    setArt(null);
  }

  async function copyShareLink() {
    const link = `${location.origin}/watch/${encodeURIComponent(roomId)}`;
    try {
      await navigator.clipboard.writeText(link);
      setCopied(true);
      window.setTimeout(() => setCopied(false), 1800);
    } catch {
      window.prompt('复制分享链接', link);
    }
  }

  function changeEpisode(index: number) {
    const episode = episodes[index];
    if (!episode || !isHost) return;
    clientRef.current?.sendCommand({ op: 'episode', episode: index, url: episode.url, positionMs: 0 });
  }

  function submitDanmaku(e: React.FormEvent) {
    e.preventDefault();
    const text = danmakuText.trim();
    if (!text) return;
    clientRef.current?.sendDanmaku(text, { color: danmakuColor, mode: 0 });
    setDanmakuText('');
  }

  async function switchSource(source: SourceRef) {
    if (!isHost || source.sourceKey === sourceKey) return;
    setSourceError('');
    setSwitchingSource(source.sourceKey);
    try {
      const detail = await api.detail(source.sourceKey, source.vodId);
      const line = detail.playLines[0];
      if (!line || line.episodes.length === 0) {
        setSourceError('该片源没有可播放的剧集');
        return;
      }
      const nextEpisodes = line.episodes;
      const index = Math.max(0, Math.min(currentEpisode, nextEpisodes.length - 1));
      const episode = nextEpisodes[index];
      const positionMs = Math.max(0, Math.round((art?.currentTime ?? 0) * 1000));
      clientRef.current?.sendCommand({
        op: 'episode',
        episode: index,
        url: episode.url,
        positionMs,
        title: roomTitle,
        sourceKey: source.sourceKey,
        vodId: source.vodId,
        sourceName: source.sourceName,
        episodes: nextEpisodes.map(ep => ({ name: ep.name, url: ep.url })),
      });
    } catch (err) {
      setSourceError(err instanceof Error ? err.message : '切换片源失败');
    } finally {
      setSwitchingSource('');
    }
  }

  if (roomGone) {
    return (
      <div className="watch-join">
        <div className="watch-join-card">
          <h1>房间已结束或不存在</h1>
          <p className="muted">房间 {roomId} 可能已被房主解散。</p>
          <button className="btn play" onClick={() => navigate('/')}>返回首页</button>
        </div>
      </div>
    );
  }

  if (!credentials) {
    return (
      <div className="watch-join">
        <div className="watch-join-card">
          <h1>加入一起看</h1>
          <p className="muted">房间 {roomId}</p>
          <form onSubmit={submitJoin}>
            <label className="watch-field">
              <span>你的昵称</span>
              <input
                value={name}
                onChange={e => setName(e.target.value)}
                placeholder="输入昵称（最多 16 个字符）"
                maxLength={16}
                autoFocus
              />
            </label>
            {joinError ? <div className="error-box">{joinError}</div> : null}
            <button className="btn play" type="submit" disabled={joining || !name.trim()}>
              {joining ? '正在加入…' : '加入房间'}
            </button>
          </form>
        </div>
      </div>
    );
  }

  const sourceOptions = dedupeSources(sources);
  if (sourceKey && !sourceOptions.some(source => source.sourceKey === sourceKey)) {
    sourceOptions.unshift({ sourceKey, sourceName, vodId, name: roomTitle });
  }
  const title = roomTitle || '一起看';
  const onlineCount = members.filter(member => member.online).length;
  const selfMember = members.find(member => member.id === credentials.memberId);
  const ownNickname = selfMember?.name || credentials.name || name;

  return (
    <div className="watch-page">
      {closedNotice ? <div className="watch-ended">{closedNotice}</div> : null}

      <header className="watch-head">
        <div className="watch-head-main">
          <h1 className="watch-title">{title}</h1>
          <span className="watch-room-id">房间 {roomId}</span>
          <span className="watch-own-name">你的昵称：{ownNickname || '—'}</span>
        </div>
        <span className={`watch-status${connected ? ' on' : ''}`}>{connected ? '已连接' : '连接中…'}</span>
        <button className="btn ghost watch-copy" onClick={copyShareLink}>
          {copied ? '✓ 已复制' : '复制分享链接'}
        </button>
        <button className="btn ghost watch-leave" onClick={handleLeave}>退出一起看</button>
      </header>

      {error ? <div className="watch-error">{error}</div> : null}

      <div className="watch-body">
        <div className="watch-stage">
          {state?.url ? (
            <Suspense fallback={<div className="center-fill"><div className="spinner" /></div>}>
              <Player
                url={state.url}
                episodeKey={String(state.episode)}
                onReady={setArt}
                danmaku
              />
            </Suspense>
          ) : (
            <div className="watch-empty">等待房主选择剧集…</div>
          )}
        </div>

        <form className="watch-danmaku-form" onSubmit={submitDanmaku}>
          <div className="watch-danmaku-colors" role="group" aria-label="弹幕颜色">
            {DANMAKU_COLORS.map(color => (
              <button
                key={color}
                type="button"
                className={`watch-danmaku-color${danmakuColor === color ? ' active' : ''}`}
                style={{ background: color }}
                aria-label={`选择颜色 ${color}`}
                aria-pressed={danmakuColor === color}
                onClick={() => setDanmakuColor(color)}
              />
            ))}
          </div>
          <input
            value={danmakuText}
            onChange={e => setDanmakuText(e.target.value)}
            placeholder="发个弹幕吧…"
            maxLength={DANMAKU_MAX_LENGTH}
            aria-label="弹幕内容"
          />
          <button className="btn play" type="submit" disabled={!danmakuText.trim()}>
            发送
          </button>
        </form>

        <aside className="watch-side">
          <div className="watch-section-head watch-members-head">
            <span>成员 ({onlineCount}/{members.length})</span>
            <span className="watch-own-name muted">你的昵称：{ownNickname || '—'}</span>
          </div>
          <ul className="watch-members">
            {members.map(member => {
              const self = member.id === credentials.memberId;
              return (
                <li
                  key={member.id}
                  className={`${member.online ? 'online' : 'offline'}${self ? ' self' : ''}`}
                >
                  <span className="watch-dot" />
                  <span className="watch-member-name">{member.name}</span>
                  {member.host ? <span className="watch-badge">房主</span> : null}
                  {member.online ? null : <span className="watch-offline">离线</span>}
                  {self ? <span className="watch-self">我</span> : null}
                </li>
              );
            })}
            {members.length === 0 ? <li className="muted">正在获取成员…</li> : null}
          </ul>

          {isHost && episodes.length > 0 ? (
            <label className="watch-field">
              <span>选集</span>
              <select value={currentEpisode} onChange={e => changeEpisode(Number(e.target.value))}>
                {episodes.map((episode, i) => (
                  <option key={episode.id || i} value={i}>
                    {episode.name || `第${episode.index || i + 1}集`}
                  </option>
                ))}
              </select>
            </label>
          ) : episodes.length > 0 ? (
            <div className="watch-current-ep">
              当前：{episodes[currentEpisode]?.name || `第${currentEpisode + 1}集`}
            </div>
          ) : null}

          <div className="watch-field watch-sources">
            <span>片源</span>
            {isHost ? (
              <>
                <div className="watch-source-chips">
                  {sourceOptions.map(source => {
                    const current = source.sourceKey === sourceKey;
                    return (
                      <button
                        key={source.sourceKey}
                        type="button"
                        className={`watch-source-chip${current ? ' active' : ''}`}
                        onClick={() => { if (!current) void switchSource(source); }}
                        disabled={current || switchingSource !== ''}
                      >
                        {source.sourceName || source.sourceKey}
                      </button>
                    );
                  })}
                  {sourcesLoading ? <span className="muted">正在查找片源…</span> : null}
                  {!sourcesLoading && sourceOptions.length === 0 ? <span className="muted">暂无其它片源</span> : null}
                </div>
                {sourceError ? <div className="watch-source-error">{sourceError}</div> : null}
              </>
            ) : (
              <div className="watch-current-source muted">{sourceName || '未知片源'}</div>
            )}
          </div>

          <div className="watch-tip">
            {isHost
              ? '你是房主，播放、暂停和拖动会同步给所有成员。'
              : '你的播放进度会自动跟随房主，手动操作会被拉回。'}
          </div>

          <button className="watch-switch" onClick={changeIdentity}>切换身份</button>
        </aside>
      </div>
    </div>
  );
}
