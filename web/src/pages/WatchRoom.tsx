import { lazy, Suspense, useEffect, useMemo, useRef, useState } from 'react';
import { useLocation, useNavigate, useParams } from 'react-router-dom';
import type Artplayer from 'artplayer';
import {
  WatchPartyClient,
  WatchPartyError,
  clearMemberCredentials,
  createDriftCorrector,
  joinRoom,
  leaveRoom,
  loadMemberCredentials,
  type StoredMemberCredentials,
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
}

const META_PREFIX = 'watch:meta:';
const NAME_KEY = 'watch:displayName';

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
  const [name, setName] = useState(() => {
    try { return localStorage.getItem(NAME_KEY) || ''; } catch { return ''; }
  });
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

  const clientRef = useRef<WatchPartyClient | null>(null);
  const initialEpisodeSent = useRef(false);

  const isHost = useMemo(
    () => !!credentials && members.some(m => m.id === credentials.memberId && m.host),
    [credentials, members],
  );

  useEffect(() => {
    if (!credentials) return;
    const client = new WatchPartyClient(roomId, credentials);
    clientRef.current = client;
    const unsubs = [
      client.on('state', s => setState(s)),
      client.on('members', list => setMembers(list)),
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
      navigate(meta?.detailPath || '/');
    }, 2000);
    return () => window.clearTimeout(timer);
  }, [closedNotice, meta?.detailPath, navigate]);

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
    });
    initialEpisodeSent.current = true;
  }, [isHost, meta, state?.url, state?.episode]);

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
    if (!meta?.title) return;
    const prev = document.title;
    document.title = `${meta.title} · 一起看`;
    return () => { document.title = prev; };
  }, [meta?.title]);

  async function submitJoin(e: React.FormEvent) {
    e.preventDefault();
    const displayName = name.trim();
    if (!displayName || joining) return;
    setJoining(true);
    setJoinError('');
    try {
      const joined = await joinRoom(roomId, displayName);
      try { localStorage.setItem(NAME_KEY, displayName); } catch { /* ignore */ }
      setState(joined.state);
      setCredentials({ memberId: joined.memberId, memberToken: joined.memberToken });
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
    navigate(meta?.detailPath || '/');
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
    const episode = meta?.episodes?.[index];
    if (!episode || !isHost) return;
    clientRef.current?.sendCommand({ op: 'episode', episode: index, url: episode.url, positionMs: 0 });
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
                placeholder="输入显示名称"
                maxLength={24}
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

  const episodes = meta?.episodes ?? [];
  const currentEpisode = state?.episode ?? meta?.episode ?? 0;
  const title = meta?.title || '一起看';

  return (
    <div className="watch-page">
      {closedNotice ? <div className="watch-ended">{closedNotice}</div> : null}

      <header className="watch-head">
        <div className="watch-head-main">
          <h1 className="watch-title">{title}</h1>
          <span className="watch-room-id">房间 {roomId}</span>
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
              />
            </Suspense>
          ) : (
            <div className="watch-empty">等待房主选择剧集…</div>
          )}
        </div>

        <aside className="watch-side">
          <div className="watch-section-head">
            <span>成员</span>
            <span className="muted">{members.filter(m => m.online).length} 在线 / {members.length}</span>
          </div>
          <ul className="watch-members">
            {members.map(member => (
              <li key={member.id} className={member.online ? 'online' : ''}>
                <span className="watch-dot" />
                <span className="watch-member-name">{member.name}</span>
                {member.host ? <span className="watch-badge">房主</span> : null}
                {member.id === credentials.memberId ? <span className="watch-self">我</span> : null}
              </li>
            ))}
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
