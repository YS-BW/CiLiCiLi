import { useEffect, useRef } from 'react';
import Artplayer, { type Setting } from 'artplayer';
import Hls, { type HlsConfig, type Level } from 'hls.js';
import artplayerPluginDanmuku from 'artplayer-plugin-danmuku';
import { PlaybackRecovery, describeHlsError } from '../lib/playback-recovery';

export type ProxyMode = 'auto' | 'direct' | 'proxy';

export interface PlayerProps {
  url: string;
  episodeKey: string;
  getRestorePosition?: () => Promise<number>;
  onProgress?: (position: number, duration: number) => void;
  onEnded?: () => void;
  onRequestSwitchSource?: (reason: string) => void;
  onHint?: (text: string) => void;
  onReady?: (art: Artplayer) => void;
  danmaku?: boolean;
}

const PROXY_MODE_KEY = 'cilicili.player.proxyMode';
const QUALITY_SETTING_NAME = 'quality';
const PROXY_SETTING_NAME = 'proxyMode';

function buildHlsConfig(): Partial<HlsConfig> {
  return {
    enableWorker: true,
    backBufferLength: 90,
    maxBufferLength: 30,
    maxMaxBufferLength: 60,
    maxBufferSize: 30 * 1000 * 1000,
    maxBufferHole: 0.5,
    startLevel: -1,
    abrEwmaDefaultEstimate: 500_000,
    fragLoadingMaxRetry: 6,
    fragLoadingRetryDelay: 1000,
    manifestLoadingMaxRetry: 3,
    manifestLoadingRetryDelay: 1000,
    appendErrorMaxRetry: 5,
  };
}

function proxyURL(mediaUrl: string): string {
  if (mediaUrl.startsWith('/api/proxy')) return mediaUrl;
  return `/api/proxy?url=${encodeURIComponent(mediaUrl)}`;
}

function loadProxyMode(): ProxyMode {
  try {
    const saved = localStorage.getItem(PROXY_MODE_KEY);
    if (saved === 'auto' || saved === 'direct' || saved === 'proxy') return saved;
  } catch { /* localStorage 不可用 */ }
  return 'auto';
}

function saveProxyMode(mode: ProxyMode): void {
  try { localStorage.setItem(PROXY_MODE_KEY, mode); } catch { /* ignore */ }
}

function proxyModeLabel(mode: ProxyMode): string {
  switch (mode) {
    case 'direct': return '强制直连';
    case 'proxy': return '强制代理';
    default: return '自动';
  }
}

function qualityLabel(level: Level, index: number): string {
  const height = Number(level.height) || 0;
  const bitrate = Number(level.bitrate) || 0;
  const name = height > 0 ? `${height}P` : (level.name || `档位 ${index + 1}`);
  if (bitrate <= 0) return name;
  return `${name} · ${(bitrate / 1_000_000).toFixed(2)} Mbps`;
}

interface HlsSession {
  destroy: () => void;
}

interface HlsSetupOptions {
  proxy: boolean;
  allowProxyFallback: boolean;
  manualLevel: number | null;
  onManualLevel: (level: number | null) => void;
  onProxyFallback: () => void;
  onFatalFailure?: (message: string) => void;
  resumeTime?: number;
  autoplay?: boolean;
}

interface StartOptions {
  resumeTime?: number;
  autoplay?: boolean;
  forceProxy?: boolean;
}

// 用现成的 ArtPlayer 作为播放器内核（UI/控制条/全屏/设置都由它提供），
// 仅用 hls.js 做 HLS 播放与「直连→代理」回退、错误恢复、续播与进度上报。
export default function Player(props: PlayerProps) {
  const hostRef = useRef<HTMLDivElement | null>(null);
  const propsRef = useRef(props);
  propsRef.current = props;

  useEffect(() => {
    const host = hostRef.current;
    if (!host || !props.url) return;

    const mediaUrl = props.url;
    let currentMode = loadProxyMode();
    let autoFellBack = false;
    let manualLevel: number | null = null;
    let session: HlsSession | null = null;
    let restored = false;

    function startSession(instance: Artplayer, video: HTMLVideoElement, opts: StartOptions = {}) {
      session?.destroy();
      session = null;

      const useProxy = currentMode === 'proxy' || opts.forceProxy === true;
      session = setupHls(video, useProxy ? proxyURL(mediaUrl) : mediaUrl, instance, {
        proxy: useProxy,
        allowProxyFallback: currentMode === 'auto' && !opts.forceProxy,
        manualLevel,
        onManualLevel: level => { manualLevel = level; },
        onProxyFallback: () => {
          if (autoFellBack) return;
          autoFellBack = true;
          const time = instance.video?.currentTime || 0;
          if (time > 0) restored = true;
          instance.notice.show = '直连失败，改用代理重试…';
          // 自动回退发生在首次拉流失败时 video 通常还是 paused，不能据此判断用户暂停，
          // 这里保持自动播放意图，避免回退后停在黑屏等用户再点一次。
          startSession(instance, instance.video, { resumeTime: time, autoplay: true, forceProxy: true });
        },
        onFatalFailure: message => {
          instance.notice.show = message;
          propsRef.current.onHint?.(message);
        },
        resumeTime: opts.resumeTime,
        autoplay: opts.autoplay,
      });
    }

    function switchMode(instance: Artplayer, mode: ProxyMode) {
      if (mode === currentMode) return;
      const resumeTime = instance.video?.currentTime || 0;
      const playing = !!instance.video && !instance.video.paused;
      currentMode = mode;
      autoFellBack = false;
      saveProxyMode(mode);
      if (resumeTime > 0) restored = true;
      instance.notice.show = `已切换为${proxyModeLabel(mode)}，正在重新拉流…`;
      startSession(instance, instance.video, { resumeTime, autoplay: playing });
    }

    const art = new Artplayer({
      container: host,
      url: mediaUrl,
      autoplay: true,
      theme: '#e50914',
      volume: 0.8,
      autoSize: false,
      autoMini: true,
      fullscreen: true,
      fullscreenWeb: true,
      pip: true,
      setting: true,
      playbackRate: true,
      aspectRatio: true,
      hotkey: true,
      lang: 'zh-cn',
      plugins: props.danmaku
        ? [
            artplayerPluginDanmuku({
              danmuku: [],
              mode: 0,
              speed: 8,
              fontSize: 25,
              opacity: 1,
              margin: [8, '18%'],
              color: '#ffffff',
              antiOverlap: true,
              emitter: false,
              maxLength: 80,
              theme: 'dark',
              visible: true,
            }),
          ]
        : [],
      customType: {
        m3u8: (video: HTMLVideoElement, _url: string, artInstance: Artplayer) => {
          startSession(artInstance, video);
        },
      },
    });

    installProxySetting(art, currentMode, mode => switchMode(art, mode));

    propsRef.current.onReady?.(art);

    // 自动播放：未静音被浏览器策略拦时，退回静音自动播放并提示开启声音
    const ensurePlay = () => {
      art.play().then(() => { /* ok */ }).catch(() => {
        art.muted = true;
        art.play().then(() => { art.notice.show = '已静音自动播放，点击音量开启声音'; }).catch(() => { /* 仍需手动点播放 */ });
      });
    };
    art.on('ready', ensurePlay);

    // 进度上报（节流 5s）
    let lastSent = 0;
    art.on('video:timeupdate', () => {
      const now = Date.now();
      if (now - lastSent < 5000) return;
      lastSent = now;
      const d = Number.isFinite(art.duration) ? art.duration : 0;
      propsRef.current.onProgress?.(art.currentTime, d);
    });
    art.on('video:pause', () => {
      const d = Number.isFinite(art.duration) ? art.duration : 0;
      propsRef.current.onProgress?.(art.currentTime, d);
    });
    art.on('video:ended', () => {
      const d = Number.isFinite(art.duration) ? art.duration : 0;
      propsRef.current.onProgress?.(art.currentTime, d);
      propsRef.current.onEnded?.();
    });

    // 续播：等时长就绪后定位
    const tryRestore = async () => {
      if (restored) return;
      const dur = art.duration;
      if (!Number.isFinite(dur) || dur <= 0) return;
      restored = true;
      try {
        const saved = (await propsRef.current.getRestorePosition?.()) ?? 0;
        if (saved > 10 && saved < dur - 2) {
          art.currentTime = saved;
          art.notice.show = `已从 ${fmt(saved)} 继续播放`;
        }
      } catch { /* ignore */ }
      ensurePlay();
    };
    art.on('ready', tryRestore);
    art.on('video:loadedmetadata', tryRestore);
    art.on('video:durationchange', tryRestore);

    // 卡顿看门狗：播放中时间轴长时间不动 → 提示可能是代理/网络拦截
    let lastCt = 0, lastTs = Date.now(), stallNotified = false;
    const watchdog = setInterval(() => {
      const v = art.video;
      if (!v || v.paused) { lastCt = v ? v.currentTime : 0; lastTs = Date.now(); return; }
      const ct = v.currentTime;
      if (Math.abs(ct - lastCt) < 0.2) {
        if (!stallNotified && Date.now() - lastTs > 8000) {
          art.notice.show = '播放卡住：可在播放器设置中切换「直连 / 代理」，或检查 Clash/系统代理';
          stallNotified = true;
        }
      } else { lastCt = ct; lastTs = Date.now(); stallNotified = false; }
    }, 2000);

    return () => {
      clearInterval(watchdog);
      session?.destroy();
      session = null;
      try { art.destroy(false); } catch { /* ignore */ }
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [props.url, props.episodeKey, props.danmaku]);

  return <div ref={hostRef} className="artplayer-host" style={{ width: '100%', height: '100%' }} />;
}

function installProxySetting(art: Artplayer, current: ProxyMode, onSelect: (mode: ProxyMode) => void) {
  const setting: Setting = {
    name: PROXY_SETTING_NAME,
    html: '播放方式',
    tooltip: proxyModeLabel(current),
    selector: [
      { html: '自动（直连失败转代理）', value: 'auto', default: current === 'auto' },
      { html: '强制直连', value: 'direct', default: current === 'direct' },
      { html: '强制代理', value: 'proxy', default: current === 'proxy' },
    ],
    onSelect(item) {
      const mode = item.value as ProxyMode;
      onSelect(mode);
      return proxyModeLabel(mode);
    },
  };
  art.setting.add(setting);
}

function installQualitySetting(
  art: Artplayer,
  hls: Hls,
  levels: Level[],
  manualLevel: number | null,
  onManualLevel: (level: number | null) => void,
) {
  if (levels.length === 0) return;
  const selector: Setting[] = [
    { html: '自动', value: 'auto', default: manualLevel == null },
    ...levels.map((level, index) => ({
      html: qualityLabel(level, index),
      value: String(index),
      default: manualLevel === index,
    })),
  ];
  const currentLabel = manualLevel != null && levels[manualLevel]
    ? qualityLabel(levels[manualLevel], manualLevel)
    : '自动';
  const setting: Setting = {
    name: QUALITY_SETTING_NAME,
    html: '画质',
    tooltip: currentLabel,
    selector,
    onSelect(item) {
      const raw = item.value;
      if (raw === 'auto') {
        hls.currentLevel = -1;
        onManualLevel(null);
        art.notice.show = '画质：自动';
        return '自动';
      }
      const index = Number(raw);
      if (Number.isInteger(index) && index >= 0 && index < levels.length) {
        hls.currentLevel = index;
        onManualLevel(index);
        art.notice.show = `画质：${item.html}`;
        return item.html;
      }
      return currentLabel;
    },
  };
  const existing = art.setting.find(QUALITY_SETTING_NAME);
  if (existing) art.setting.remove(QUALITY_SETTING_NAME);
  art.setting.add(setting);
}

function setupHls(video: HTMLVideoElement, mediaUrl: string, art: Artplayer, opts: HlsSetupOptions): HlsSession {
  const recovery = new PlaybackRecovery();
  const hls = new Hls(buildHlsConfig());
  (art as any).hls = hls;

  let disposed = false;
  let levels: Level[] = [];
  let manualLevel = opts.manualLevel;
  let resumeApplied = false;
  let directFailureNotified = false;

  const applyResumeAndPlay = () => {
    if (resumeApplied) return;
    if (opts.resumeTime && opts.resumeTime > 0) {
      if (!Number.isFinite(video.duration) || video.duration <= 0) return;
      resumeApplied = true;
      try { video.currentTime = Math.min(opts.resumeTime, Math.max(0, video.duration - 0.5)); } catch { /* ignore */ }
    } else {
      resumeApplied = true;
    }
    if (opts.autoplay) art.play().catch(() => { /* ignore */ });
  };
  video.addEventListener('loadedmetadata', applyResumeAndPlay);

  hls.on(Hls.Events.MANIFEST_PARSED, (_e, data) => {
    if (disposed) return;
    recovery.markHealthy();
    levels = data.levels || [];
    if (manualLevel != null && manualLevel >= levels.length) {
      manualLevel = null;
      opts.onManualLevel(null);
    }
    installQualitySetting(art, hls, levels, manualLevel, level => {
      manualLevel = level;
      opts.onManualLevel(level);
    });
    if (manualLevel != null && manualLevel >= 0 && manualLevel < levels.length) {
      hls.currentLevel = manualLevel;
    }
    applyResumeAndPlay();
  });

  hls.on(Hls.Events.LEVEL_SWITCHED, (_e, data) => {
    if (disposed) return;
    const setting = art.setting.find(QUALITY_SETTING_NAME);
    if (!setting) return;
    const level = levels[data.level];
    const label = level ? qualityLabel(level, data.level) : `档位 ${data.level + 1}`;
    setting.tooltip = hls.autoLevelEnabled ? `自动 · ${label}` : label;
  });

  hls.on(Hls.Events.FRAG_LOADED, () => recovery.markHealthy());

  hls.on(Hls.Events.ERROR, (_e, data) => {
    if (disposed || !data.fatal) return;
    const decision = recovery.onFatal(data.type, data.details);
    switch (decision.action) {
      case 'ignore': break;
      case 'retry': {
        if (opts.allowProxyFallback && !opts.proxy && !mediaUrl.startsWith('/api/proxy')
          && (decision.attempt >= 2 || data.details === 'manifestLoadError')) {
          opts.onProxyFallback();
          return;
        }
        if (!opts.allowProxyFallback && !opts.proxy) {
          // 强制直连失败：明确告知大概率是 CORS/防盗链，避免用户以为播放器卡死。
          if (!directFailureNotified) {
            directFailureNotified = true;
            opts.onFatalFailure?.('直连失败：可能是 CORS/防盗链限制，请在播放器设置中切换为「代理」或「自动」');
          }
        } else if (opts.proxy) {
          art.notice.show = `代理请求失败（第 ${decision.attempt} 次）…`;
        } else {
          art.notice.show = `${decision.reason}（第 ${decision.attempt} 次）…`;
        }
        recovery.schedule(decision.delayMs, () => hls.startLoad());
        break;
      }
      case 'recover-media': {
        art.notice.show = `${decision.reason}（第 ${decision.attempt} 次）…`;
        if (decision.swapAudio) hls.swapAudioCodec();
        hls.recoverMediaError();
        break;
      }
      case 'switch-source': {
        const message = opts.proxy
          ? `代理播放失败：${describeHlsError(data.type, data.details)}`
          : (!opts.allowProxyFallback
            ? '直连失败：可能是 CORS/防盗链限制，请在播放器设置中切换为「代理」或「自动」'
            : `${describeHlsError(data.type, data.details)}：${decision.reason}`);
        art.notice.show = message;
        if (!opts.allowProxyFallback) opts.onFatalFailure?.(message);
        break;
      }
    }
  });

  hls.loadSource(mediaUrl);
  hls.attachMedia(video);

  return {
    destroy() {
      if (disposed) return;
      disposed = true;
      video.removeEventListener('loadedmetadata', applyResumeAndPlay);
      try { hls.destroy(); } catch { /* ignore */ }
      if ((art as any).hls === hls) delete (art as any).hls;
    },
  };
}

function fmt(sec: number): string {
  const s = Math.max(0, Math.floor(sec));
  const m = Math.floor(s / 60);
  const r = s % 60;
  return `${m}:${String(r).padStart(2, '0')}`;
}