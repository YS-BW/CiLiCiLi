import { forwardRef, useEffect, useImperativeHandle, useRef } from 'react';
import Hls, { type HlsConfig } from 'hls.js';
import { PlaybackRecovery, describeHlsError } from '../lib/playback-recovery';

export interface PlayerHandle {
  seek: (seconds: number) => void;
  getCurrentTime: () => number;
  getDuration: () => number;
  pause: () => void;
  play: () => void;
}
export const original_duration_placeholder = null;

export interface PlayerProps {
  url: string;                 // 当前集播放地址（直连 m3u8，或已含 /api/proxy）
  episodeKey: string;          // 用于区分换集
  getRestorePosition?: () => Promise<number>; // 续播位置（秒）
  onProgress?: (position: number, duration: number) => void;
  onEnded?: () => void;
  onRequestSwitchSource?: (reason: string) => void;
  onHint?: (text: string) => void;
  onError?: (text: string) => void;
}

// hls.js 配置：快起播 + 少卡顿（对齐 LibreTV-Next，去掉广告过滤/缓存）
function buildHlsConfig(): Partial<HlsConfig> {
  return {
    debug: false,
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

function toProxyUrl(mediaUrl: string): string {
  if (mediaUrl.startsWith('/api/proxy')) return mediaUrl;
  return `/api/proxy?url=${encodeURIComponent(mediaUrl)}`;
}

const Player = forwardRef<PlayerHandle, PlayerProps>(function Player(props, ref) {
  const videoRef = useRef<HTMLVideoElement | null>(null);
  const hlsRef = useRef<Hls | null>(null);
  const recoveryRef = useRef<PlaybackRecovery | null>(null);
  const playbackStartedRef = useRef(false);
  const restoredRef = useRef(false);
  const propsRef = useRef(props);
  propsRef.current = props;

  useImperativeHandle(ref, () => ({
    seek: (s: number) => { const v = videoRef.current; if (v) v.currentTime = s; },
    getCurrentTime: () => videoRef.current?.currentTime ?? 0,
    getDuration: () => videoRef.current?.duration ?? 0,
    pause: () => videoRef.current?.pause(),
    play: () => { videoRef.current?.play().catch(() => {}); },
  }), []);

  // 进度上报：节流 + 暂停/结束时补报
  useEffect(() => {
    const v = videoRef.current;
    if (!v) return;
    let lastSent = 0;
    const report = () => {
      const p = propsRef.current;
      if (!p.onProgress) return;
      const dur = Number.isFinite(v.duration) ? v.duration : 0;
      p.onProgress(v.currentTime, dur);
      lastSent = Date.now();
    };
    const onTime = () => { if (Date.now() - lastSent > 5000) report(); };
    const onPause = () => report();
    const onEnded = () => { report(); propsRef.current.onEnded?.(); };
    v.addEventListener('timeupdate', onTime);
    v.addEventListener('pause', onPause);
    v.addEventListener('ended', onEnded);
    return () => {
      v.removeEventListener('timeupdate', onTime);
      v.removeEventListener('pause', onPause);
      v.removeEventListener('ended', onEnded);
    };
  }, []);

  // 换源/换集：重建 hls（不重建 video）
  useEffect(() => {
    const video = videoRef.current;
    if (!video || !props.url) return;

    playbackStartedRef.current = false;
    restoredRef.current = false;
    recoveryRef.current?.dispose();
    recoveryRef.current = new PlaybackRecovery();

    setupHls(video, props.url, true);

    return () => {
      recoveryRef.current?.dispose();
      recoveryRef.current = null;
      hlsRef.current?.destroy();
      hlsRef.current = null;
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [props.url, props.episodeKey]);

  function finishRestore() {
    const video = videoRef.current;
    const p = propsRef.current;
    if (!video || restoredRef.current) return;
    const duration = Number.isFinite(video.duration) ? video.duration : 0;
    if (duration <= 0) return; // duration 未就绪，等后续事件再试
    (async () => {
      try {
        const saved = (await p.getRestorePosition?.()) ?? 0;
        const curDur = Number.isFinite(video.duration) ? video.duration : duration;
        if (saved > 10 && saved < curDur - 2) {
          video.currentTime = saved;
          p.onHint?.(`已从 ${fmt(saved)} 继续播放`);
        }
      } catch { /* IndexedDB 之外的失败不阻塞 */ }
      restoredRef.current = true;
      video.play().catch(() => {});
    })();
  }

  function setupHls(video: HTMLVideoElement, mediaUrl: string, allowProxyFallback: boolean) {
    const p = propsRef.current;
    hlsRef.current?.destroy();
    const hls = new Hls(buildHlsConfig());
    hlsRef.current = hls;

    const onMeta = () => finishRestore();
    const restoreEvents = ['loadedmetadata', 'durationchange', 'canplay'] as const;
    restoreEvents.forEach(e => video.addEventListener(e, onMeta));

    hls.loadSource(mediaUrl);
    hls.attachMedia(video);

    hls.on(Hls.Events.MANIFEST_PARSED, () => {
      recoveryRef.current?.markHealthy();
      finishRestore();
    });
    hls.on(Hls.Events.FRAG_LOADED, () => recoveryRef.current?.markHealthy());
    hls.on(Hls.Events.FRAG_BUFFERED, () => { playbackStartedRef.current = true; });

    hls.on(Hls.Events.ERROR, (_evt, data) => {
      if (!data.fatal) return;
      const recovery = recoveryRef.current;
      if (!recovery) return;
      const decision = recovery.onFatal(data.type, data.details);
      switch (decision.action) {
        case 'ignore': break;
        case 'retry': {
          // 前 2 次保留直连；到阈值或清单级错误时升级为代理（同源 cookie）
          if (allowProxyFallback && !mediaUrl.startsWith('/api/proxy')
            && (decision.attempt >= 2 || data.details === 'manifestLoadError')) {
            p.onHint?.('直连失败，改用代理重试…');
            restoreEvents.forEach(e => video.removeEventListener(e, onMeta));
            setupHls(video, toProxyUrl(mediaUrl), false);
            return;
          }
          p.onHint?.(`${decision.reason}（第 ${decision.attempt} 次）…`);
          recovery.schedule(decision.delayMs, () => hls.startLoad());
          break;
        }
        case 'recover-media': {
          p.onHint?.(`${decision.reason}（第 ${decision.attempt} 次）…`);
          if (decision.swapAudio) hls.swapAudioCodec();
          hls.recoverMediaError();
          break;
        }
        case 'switch-source': {
          const message = `${describeHlsError(data.type, data.details)}：${decision.reason}`;
          if (playbackStartedRef.current) {
            p.onHint?.(message);
            p.onRequestSwitchSource?.(decision.reason);
          } else {
            p.onError?.(`视频加载失败，${message}，请尝试其他片源`);
          }
          break;
        }
      }
    });
  }

  return (
    <div className="player-wrap">
      <video ref={videoRef} className="player-video" controls playsInline preload="metadata" />
    </div>
  );
});

export default Player;

function fmt(sec: number): string {
  const s = Math.max(0, Math.floor(sec));
  const h = Math.floor(s / 3600);
  const m = Math.floor((s % 3600) / 60);
  const r = s % 60;
  return h > 0
    ? `${h}:${String(m).padStart(2, '0')}:${String(r).padStart(2, '0')}`
    : `${m}:${String(r).padStart(2, '0')}`;
}
