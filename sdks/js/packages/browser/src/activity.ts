import { perfNow } from './env';

/**
 * Tracks "something happened" signals: DOM mutations, navigations and network requests.
 * Used for dead-click detection and SPA view loading time (settle).
 */
export class Activity {
  /** performance.now() of the last activity. */
  last = 0;
  pending = 0;
  private readonly listeners = new Set<() => void>();
  private observer: MutationObserver | undefined;

  start(): void {
    if (typeof MutationObserver === 'undefined' || typeof document === 'undefined') return;
    this.observer = new MutationObserver(() => this.ping());
    this.observer.observe(document.documentElement, { childList: true, subtree: true, attributes: true, characterData: true });
  }

  stop(): void {
    this.observer?.disconnect();
    this.listeners.clear();
  }

  ping = (): void => {
    this.last = perfNow();
    this.listeners.forEach((f) => f());
  };

  requestStart = (): void => {
    this.pending++;
    this.ping();
  };

  requestEnd = (): void => {
    this.pending = Math.max(0, this.pending - 1);
    this.ping();
  };

  subscribe(f: () => void): () => void {
    this.listeners.add(f);
    return () => this.listeners.delete(f);
  }
}
