export interface WatchState {
  seq: number;
  paused: boolean;
  positionMs: number;
  serverTimeMs: number;
  rate: number;
  episode: number;
  url: string;
  contentOffsetMs: number;
}

export interface WatchMember {
  id: string;
  name: string;
  host: boolean;
  online: boolean;
}

export interface WatchCredentials {
  roomId: string;
  memberId: string;
  memberToken: string;
  state: WatchState;
}

export interface StoredMemberCredentials {
  memberId: string;
  memberToken: string;
}

export class WatchPartyError extends Error {
  status: number;

  constructor(message: string, status: number) {
    super(message);
    this.name = 'WatchPartyError';
    this.status = status;
  }
}

const STORAGE_PREFIX = 'wt:';

function storageKey(roomId: string): string {
  return STORAGE_PREFIX + roomId;
}

export function saveMemberCredentials(roomId: string, credentials: StoredMemberCredentials): void {
  try {
    localStorage.setItem(storageKey(roomId), JSON.stringify({
      memberId: credentials.memberId,
      memberToken: credentials.memberToken,
    }));
  } catch {
    // localStorage 不可用时保持内存态即可。
  }
}

export function loadMemberCredentials(roomId: string): StoredMemberCredentials | null {
  try {
    const raw = localStorage.getItem(storageKey(roomId));
    if (!raw) return null;
    const parsed = JSON.parse(raw) as Partial<StoredMemberCredentials>;
    if (typeof parsed.memberId !== 'string' || typeof parsed.memberToken !== 'string') return null;
    return { memberId: parsed.memberId, memberToken: parsed.memberToken };
  } catch {
    return null;
  }
}

export function clearMemberCredentials(roomId: string): void {
  try {
    localStorage.removeItem(storageKey(roomId));
  } catch {
    // ignore
  }
}

async function parseError(resp: Response): Promise<WatchPartyError> {
  let message = `请求失败 (HTTP ${resp.status})`;
  try {
    const body = await resp.json() as { error?: string };
    if (body?.error) message = body.error;
  } catch {
    // ignore
  }
  return new WatchPartyError(message, resp.status);
}

async function postJSON<T>(path: string, body: unknown, authToken?: string): Promise<T> {
  const headers: Record<string, string> = { 'Content-Type': 'application/json' };
  if (authToken) headers.Authorization = `Bearer ${authToken}`;
  const resp = await fetch(path, {
    method: 'POST',
    credentials: 'include',
    headers,
    body: JSON.stringify(body),
  });
  if (!resp.ok) throw await parseError(resp);
  const text = await resp.text();
  if (!text) return undefined as unknown as T;
  return JSON.parse(text) as T;
}

export async function createRoom(name = '房主', authToken?: string): Promise<WatchCredentials> {
  const credentials = await postJSON<WatchCredentials>('/api/watch/rooms', { name }, authToken);
  saveMemberCredentials(credentials.roomId, credentials);
  return credentials;
}

export async function joinRoom(roomId: string, name = '观众'): Promise<WatchCredentials> {
  const credentials = await postJSON<WatchCredentials>(
    `/api/watch/rooms/${encodeURIComponent(roomId)}/join`,
    { name },
  );
  saveMemberCredentials(roomId, credentials);
  return credentials;
}

export type WatchCommandOp = 'play' | 'pause' | 'seek' | 'episode';

export interface WatchCommand {
  op: WatchCommandOp;
  positionMs?: number;
  episode?: number;
  url?: string;
}

interface ServerStateMessage {
  type: 'state';
  state: WatchState;
}

interface ServerMembersMessage {
  type: 'members';
  members: WatchMember[];
}

interface ServerPongMessage {
  type: 'pong';
  clientTs: number;
  serverTs: number;
}

interface ServerErrorMessage {
  type: 'error';
  msg: string;
}

type ServerMessage = ServerStateMessage | ServerMembersMessage | ServerPongMessage | ServerErrorMessage;

interface ClientMessage {
  type: 'ping' | 'cmd' | 'buffering';
  clientTs?: number;
  op?: WatchCommandOp;
  positionMs?: number;
  episode?: number;
  url?: string;
  state?: boolean;
}

export interface WatchClientEvents {
  state: WatchState;
  members: WatchMember[];
  error: string;
  open: void;
  close: { code: number; reason: string; willReconnect: boolean };
}

type Listener<T> = (payload: T) => void;

const PING_INTERVAL_MS = 2000;
const MAX_RTT_SAMPLES = 8;
const RECONNECT_BASE_MS = 500;
const RECONNECT_MAX_MS = 15000;
const OUTBOX_LIMIT = 128;

export class WatchPartyClient {
  readonly roomId: string;

  private readonly memberId: string;
  private readonly memberToken: string;
  private socket: WebSocket | null = null;
  private pingTimer: ReturnType<typeof setInterval> | null = null;
  private reconnectTimer: ReturnType<typeof setTimeout> | null = null;
  private reconnectAttempt = 0;
  private manuallyClosed = false;
  private lastSeq = -1;
  private currentState: WatchState | null = null;
  private outbox: string[] = [];
  private offset = Date.now() - performance.now();
  private rttSamples: Array<{ rtt: number; offset: number }> = [];
  private listeners = new Map<keyof WatchClientEvents, Set<Listener<never>>>();

  constructor(roomId: string, credentials?: StoredMemberCredentials) {
    this.roomId = roomId;
    const stored = credentials ?? loadMemberCredentials(roomId);
    if (!stored) throw new WatchPartyError('缺少房间成员凭证', 401);
    this.memberId = stored.memberId;
    this.memberToken = stored.memberToken;
  }

  on<K extends keyof WatchClientEvents>(event: K, listener: Listener<WatchClientEvents[K]>): () => void {
    let set = this.listeners.get(event);
    if (!set) {
      set = new Set();
      this.listeners.set(event, set);
    }
    const typed = listener as Listener<never>;
    set.add(typed);
    return () => { set?.delete(typed); };
  }

  connect(): void {
    this.manuallyClosed = false;
    this.openSocket();
  }

  disconnect(): void {
    this.manuallyClosed = true;
    this.clearTimers();
    if (this.socket) {
      const socket = this.socket;
      this.socket = null;
      try { socket.close(1000, 'client close'); } catch { /* ignore */ }
    }
  }

  getState(): WatchState | null {
    return this.currentState;
  }

  getClockOffset(): number {
    return this.offset;
  }

  getRtt(): number {
    return this.rttSamples.length ? this.minRttSample().rtt : 0;
  }

  serverNow(): number {
    return performance.now() + this.offset;
  }

  expectedPositionMs(state: WatchState): number {
    const rate = state.rate > 0 ? state.rate : 1;
    const delta = state.paused ? 0 : this.serverNow() - state.serverTimeMs;
    return state.positionMs + delta * rate;
  }

  sendCommand(command: WatchCommand): void {
    this.send({
      type: 'cmd',
      op: command.op,
      positionMs: command.positionMs ?? 0,
      episode: command.episode ?? 0,
      url: command.url ?? '',
    });
  }

  sendBuffering(buffering: boolean): void {
    this.send({ type: 'buffering', state: buffering });
  }

  private openSocket(): void {
    if (this.manuallyClosed) return;
    if (this.socket && (this.socket.readyState === WebSocket.OPEN || this.socket.readyState === WebSocket.CONNECTING)) {
      return;
    }

    const protocol = location.protocol === 'https:' ? 'wss:' : 'ws:';
    const query = new URLSearchParams({
      memberId: this.memberId,
      token: this.memberToken,
    });
    const url = `${protocol}//${location.host}/api/watch/rooms/${encodeURIComponent(this.roomId)}/ws?${query.toString()}`;

    let socket: WebSocket;
    try {
      socket = new WebSocket(url);
    } catch (err) {
      this.emit('error', err instanceof Error ? err.message : 'WebSocket 连接失败');
      this.scheduleReconnect();
      return;
    }

    this.socket = socket;
    this.lastSeq = -1;

    socket.onopen = () => {
      if (this.socket !== socket) return;
      this.reconnectAttempt = 0;
      this.emit('open', undefined);
      this.flushOutbox();
      this.startPing();
    };

    socket.onmessage = (event: MessageEvent) => {
      if (this.socket !== socket) return;
      if (typeof event.data !== 'string') return;
      let message: ServerMessage;
      try {
        message = JSON.parse(event.data) as ServerMessage;
      } catch {
        return;
      }
      this.handleMessage(message);
    };

    socket.onerror = () => {
      if (this.socket !== socket) return;
      this.emit('error', 'WebSocket 连接错误');
    };

    socket.onclose = (event: CloseEvent) => {
      if (this.socket !== socket) return;
      this.socket = null;
      this.stopPing();
      const willReconnect = !this.manuallyClosed;
      this.emit('close', { code: event.code, reason: event.reason, willReconnect });
      if (willReconnect) this.scheduleReconnect();
    };
  }

  private handleMessage(message: ServerMessage): void {
    switch (message.type) {
      case 'state': {
        const state = message.state;
        if (!state || typeof state.seq !== 'number') return;
        if (state.seq <= this.lastSeq) return;
        this.lastSeq = state.seq;
        this.currentState = state;
        this.emit('state', state);
        return;
      }
      case 'members':
        this.emit('members', Array.isArray(message.members) ? message.members : []);
        return;
      case 'pong':
        this.handlePong(message);
        return;
      case 'error':
        this.emit('error', message.msg || '未知错误');
        return;
    }
  }

  private handlePong(message: ServerPongMessage): void {
    const now = performance.now();
    const rtt = Math.max(0, now - message.clientTs);
    const offset = message.serverTs - (message.clientTs + rtt / 2);
    this.rttSamples.push({ rtt, offset });
    if (this.rttSamples.length > MAX_RTT_SAMPLES) this.rttSamples.shift();
    this.offset = this.minRttSample().offset;
  }

  private minRttSample(): { rtt: number; offset: number } {
    let best = this.rttSamples[0];
    for (const sample of this.rttSamples) {
      if (sample.rtt < best.rtt) best = sample;
    }
    return best;
  }

  private startPing(): void {
    this.stopPing();
    this.sendPing();
    this.pingTimer = setInterval(() => this.sendPing(), PING_INTERVAL_MS);
  }

  private stopPing(): void {
    if (this.pingTimer !== null) {
      clearInterval(this.pingTimer);
      this.pingTimer = null;
    }
  }

  private sendPing(): void {
    if (this.socket?.readyState !== WebSocket.OPEN) return;
    this.send({ type: 'ping', clientTs: Math.round(performance.now()) });
  }

  private scheduleReconnect(): void {
    if (this.manuallyClosed || this.reconnectTimer !== null) return;
    this.reconnectAttempt += 1;
    const base = Math.min(RECONNECT_BASE_MS * 2 ** (this.reconnectAttempt - 1), RECONNECT_MAX_MS);
    const delay = base + Math.floor(Math.random() * base * 0.25);
    this.reconnectTimer = setTimeout(() => {
      this.reconnectTimer = null;
      this.openSocket();
    }, delay);
  }

  private clearTimers(): void {
    this.stopPing();
    if (this.reconnectTimer !== null) {
      clearTimeout(this.reconnectTimer);
      this.reconnectTimer = null;
    }
  }

  private send(message: ClientMessage): void {
    const data = JSON.stringify(message);
    if (this.socket?.readyState === WebSocket.OPEN) {
      this.socket.send(data);
      return;
    }
    this.outbox.push(data);
    if (this.outbox.length > OUTBOX_LIMIT) this.outbox.shift();
  }

  private flushOutbox(): void {
    if (this.socket?.readyState !== WebSocket.OPEN) return;
    const pending = this.outbox;
    this.outbox = [];
    for (const data of pending) this.socket.send(data);
  }

  private emit<K extends keyof WatchClientEvents>(event: K, payload: WatchClientEvents[K]): void {
    const set = this.listeners.get(event);
    if (!set) return;
    const typed = payload as never;
    for (const listener of Array.from(set)) {
      try {
        (listener as Listener<WatchClientEvents[K]>)(typed);
      } catch {
        // 单个监听器异常不应影响其它监听器。
      }
    }
  }
}

export interface DriftPlayer {
  currentTime: number;
  playbackRate: number;
  paused: boolean;
  play(): Promise<void> | void;
  pause(): void;
}

export interface DriftCorrectorOptions {
  intervalMs?: number;
  ignoreThresholdMs?: number;
  hardSeekThresholdMs?: number;
  onSeek?: (mediaPositionMs: number, driftMs: number) => void;
}

export interface DriftCorrector {
  start(): void;
  stop(): void;
  tick(): void;
}

export function createDriftCorrector(
  player: DriftPlayer,
  client: WatchPartyClient,
  options: DriftCorrectorOptions = {},
): DriftCorrector {
  const intervalMs = options.intervalMs ?? 500;
  const ignoreThresholdMs = options.ignoreThresholdMs ?? 150;
  const hardSeekThresholdMs = options.hardSeekThresholdMs ?? 2000;
  let timer: ReturnType<typeof setInterval> | null = null;
  let state: WatchState | null = client.getState();
  const unsubscribe = client.on('state', next => { state = next; });

  const tick = (): void => {
    const current = state;
    if (!current) return;

    if (current.paused && !player.paused) player.pause();

    const expected = client.expectedPositionMs(current);
    const contentOffset = current.contentOffsetMs || 0;
    const actual = player.currentTime * 1000 - contentOffset;
    const drift = expected - actual;
    const absDrift = Math.abs(drift);
    const rate = current.rate > 0 ? current.rate : 1;

    if (absDrift < ignoreThresholdMs) {
      if (Math.abs(player.playbackRate - rate) > 0.001) player.playbackRate = rate;
    } else if (absDrift < hardSeekThresholdMs) {
      const targetRate = drift > 0 ? 1.05 : 0.95;
      if (Math.abs(player.playbackRate - targetRate) > 0.001) player.playbackRate = targetRate;
    } else {
      const mediaPositionMs = Math.max(0, expected + contentOffset);
      player.currentTime = mediaPositionMs / 1000;
      player.playbackRate = rate;
      options.onSeek?.(mediaPositionMs, drift);
    }

    if (!current.paused && player.paused) {
      try {
        void Promise.resolve(player.play()).catch(() => { /* 自动播放可能被浏览器拦截 */ });
      } catch {
        // ignore
      }
    }
  };

  return {
    tick,
    start(): void {
      if (timer !== null) return;
      tick();
      timer = setInterval(tick, intervalMs);
    },
    stop(): void {
      if (timer !== null) {
        clearInterval(timer);
        timer = null;
      }
      unsubscribe();
    },
  };
}
