import { useEffect, useRef } from 'react';
import Artplayer from 'artplayer';
import Hls, { type HlsConfig } from 'hls.js';
import { PlaybackRecovery, describeHlsError } from '../lib/playback-recovery';

export interface PlayerProps {
  url: string;
  episodeKey: string;
  getRestorePosition?: () => Promise<number>;
  onProgress?: (position: number, duration: number) => void;
  onEnded?: () => void;
  onRequestSwitchSource?: (reason: string) => void;
  onHint?: (text: string) => void;
  onReady?: (art: Artplayer) => void;
}

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

// 用现成的 ArtPlayer 作为播放器内核（UI/控制条/全屏/设置都由它提供），
// 仅用 hls.js 做 HLS 播放与「直连→代理」回退、错误恢复、续播与进度上报。
export default function Player(props: PlayerProps) {
  const hostRef = useRef<HTMLDivElement | null>(null);
  const propsRef = useRef(props);
  propsRef.current = props;

  useEffect(() => {
    const host = hostRef.current;
    if (!host || !props.url) return;

    const art = new Artplayer({
      container: host,
      url: props.url,
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
      customType: {
        m3u8: (video: HTMLVideoElement, url: string, artInstance: Artplayer) => {
          setupHls(video, url, artInstance, true);
        },
      },
    });

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
    let restored = false;
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
          art.notice.show = '播放卡住：若使用 Clash/代理，请把视频源域名设为直连，或关闭系统代理后重试';
          stallNotified = true;
        }
      } else { lastCt = ct; lastTs = Date.now(); stallNotified = false; }
    }, 2000);

    return () => { clearInterval(watchdog); try { art.destroy(false); } catch { /* ignore */ } };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [props.url, props.episodeKey]);

  return <div ref={hostRef} className="artplayer-host" style={{ width: '100%', height: '100%' }} />;
}

function setupHls(video: HTMLVideoElement, mediaUrl: string, art: Artplayer, allowProxyFallback: boolean) {
  const recovery = new PlaybackRecovery();
  const hls = new Hls(buildHlsConfig());
  (art as any).hls = hls;
  hls.loadSource(mediaUrl);
  hls.attachMedia(video);

  hls.on(Hls.Events.MANIFEST_PARSED, () => recovery.markHealthy());
  hls.on(Hls.Events.FRAG_LOADED, () => recovery.markHealthy());

  hls.on(Hls.Events.ERROR, (_e, data) => {
    if (!data.fatal) return;
    const decision = recovery.onFatal(data.type, data.details);
    switch (decision.action) {
      case 'ignore': break;
      case 'retry': {
        if (allowProxyFallback && !mediaUrl.startsWith('/api/proxy')
          && (decision.attempt >= 2 || data.details === 'manifestLoadError')) {
          art.notice.show = '直连失败，改用代理重试…';
          hls.destroy();
          setupHls(video, proxyURL(mediaUrl), art, false);
          return;
        }
        art.notice.show = `${decision.reason}（第 ${decision.attempt} 次）…`;
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
        art.notice.show = `${describeHlsError(data.type, data.details)}：${decision.reason}`;
        break;
      }
    }
  });

  art.once('destroy', () => { try { hls.destroy(); } catch { /* ignore */ } });
}

function fmt(sec: number): string {
  const s = Math.max(0, Math.floor(sec));
  const m = Math.floor(s / 60);
  const r = s % 60;
  return `${m}:${String(r).padStart(2, '0')}`;
}
