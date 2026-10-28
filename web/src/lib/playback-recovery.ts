// HLS 播放错误恢复决策器。
// 只做决策、不碰 hls 实例 —— 可独立理解/测试，调用方拿到 decision 后自行执行。
// 移植自 LibreTV-Next 的 playback-recovery 思路：计数=连续失败次数，收到 FRAG_LOADED 视为恢复。

export const HLS_ERROR_NETWORK = 'networkError';
export const HLS_ERROR_MEDIA = 'mediaError';

// 格式/编码层面的硬失败，重试无意义，直接换源。
const HARD_FAIL_DETAILS = new Set([
  'manifestParsingError',
  'manifestIncompatibleCodecsError',
  'levelEmptyError',
]);

export interface RecoveryPolicy {
  maxNetworkRetries: number;
  maxMediaRecoveries: number;
  baseDelayMs: number;
  maxDelayMs: number;
}

export const DEFAULT_RECOVERY_POLICY: RecoveryPolicy = {
  maxNetworkRetries: 3,
  maxMediaRecoveries: 2,
  baseDelayMs: 800,
  maxDelayMs: 8000,
};

export type RecoveryDecision =
  | { action: 'retry'; delayMs: number; attempt: number; reason: string }
  | { action: 'recover-media'; swapAudio: boolean; attempt: number; reason: string }
  | { action: 'switch-source'; reason: string }
  | { action: 'ignore'; reason: string };

const HEALTHY_QUIET_MS = 3000;

export class PlaybackRecovery {
  private policy: RecoveryPolicy;
  private networkRetries = 0;
  private mediaRecoveries = 0;
  private lastActionAt = 0;
  private disposed = false;
  private timers = new Set<ReturnType<typeof setTimeout>>();

  constructor(policy: Partial<RecoveryPolicy> = {}) {
    this.policy = { ...DEFAULT_RECOVERY_POLICY, ...policy };
  }

  markHealthy(): void {
    if (this.disposed) return;
    if (this.lastActionAt && Date.now() - this.lastActionAt < HEALTHY_QUIET_MS) return;
    this.networkRetries = 0;
    this.mediaRecoveries = 0;
  }

  onFatal(type: string, details: string): RecoveryDecision {
    if (this.disposed) return { action: 'ignore', reason: '已销毁' };
    this.lastActionAt = Date.now();

    if (HARD_FAIL_DETAILS.has(details)) {
      return { action: 'switch-source', reason: '播放列表格式无法解析，重试无意义' };
    }
    if (type === HLS_ERROR_MEDIA) {
      this.mediaRecoveries += 1;
      if (this.mediaRecoveries > this.policy.maxMediaRecoveries) {
        return { action: 'switch-source', reason: '媒体解码错误多次恢复无效' };
      }
      return {
        action: 'recover-media',
        swapAudio: this.mediaRecoveries >= 2,
        attempt: this.mediaRecoveries,
        reason: '媒体解码错误，尝试恢复',
      };
    }
    if (type === HLS_ERROR_NETWORK) {
      this.networkRetries += 1;
      if (this.networkRetries > this.policy.maxNetworkRetries) {
        return { action: 'switch-source', reason: `网络错误已重试 ${this.policy.maxNetworkRetries} 次仍未恢复` };
      }
      return {
        action: 'retry',
        attempt: this.networkRetries,
        delayMs: Math.min(this.policy.baseDelayMs * 2 ** (this.networkRetries - 1), this.policy.maxDelayMs),
        reason: '网络错误，退避重试',
      };
    }
    return { action: 'switch-source', reason: `未知的致命错误类型：${type}` };
  }

  schedule(delayMs: number, task: () => void): void {
    const timer = setTimeout(() => {
      this.timers.delete(timer);
      if (!this.disposed) task();
    }, delayMs);
    this.timers.add(timer);
  }

  dispose(): void {
    this.disposed = true;
    for (const t of this.timers) clearTimeout(t);
    this.timers.clear();
  }
}

export function describeHlsError(type: string, details: string): string {
  if (HARD_FAIL_DETAILS.has(details)) return '播放列表格式无法解析';
  switch (details) {
    case 'manifestLoadError':
    case 'manifestLoadTimeOut': return '播放列表加载失败';
    case 'manifestParsingError': return '播放列表解析失败';
    case 'fragLoadError':
    case 'fragLoadTimeOut': return '视频分片加载失败';
    case 'keyLoadError': return '解密密钥加载失败';
    case 'bufferStalledError': return '缓冲不足';
    default: return type === HLS_ERROR_MEDIA ? '媒体解码错误' : `播放错误（${details || type}）`;
  }
}
