/**
 * Per-session replay state shared by all tabs of a session (contracts.md §23): the sampling decision and the next segment
 * number, under `tt_replay:<session_id>`. localStorage (shared, like the RUM session) → sessionStorage → memory.
 */

export type StoredMode = 'off' | 'buffer' | 'session';

export interface Entry {
  /** Sampling decision. */
  m: StoredMode;
  /** Next segment number to hand out. */
  n: number;
}

export const KEY_PREFIX = 'tt_replay:';

const probe = (get: () => Storage): Storage | undefined => {
  try {
    const s = get();
    s.setItem('tt_replay_probe', '1');
    s.removeItem('tt_replay_probe');
    return s;
  } catch {
    return undefined;
  }
};

const isMode = (m: unknown): m is StoredMode => m === 'off' || m === 'buffer' || m === 'session';

export class ReplayStore {
  private readonly storage: Storage | undefined;
  private readonly mem = new Map<string, string>();

  constructor() {
    this.storage = probe(() => window.localStorage) ?? probe(() => window.sessionStorage);
  }

  /** Which backend is in use (for tests / debugging). */
  get kind(): 'local' | 'session' | 'memory' {
    if (!this.storage) return 'memory';
    try {
      return this.storage === window.localStorage ? 'local' : 'session';
    } catch {
      return 'session';
    }
  }

  private read(key: string): string | null {
    if (this.storage) {
      try {
        return this.storage.getItem(key);
      } catch {
        /* fall through to memory */
      }
    }
    return this.mem.get(key) ?? null;
  }

  private write(key: string, v: string): void {
    this.mem.set(key, v); // memory mirror: survives a storage that starts throwing (quota) for this page
    try {
      this.storage?.setItem(key, v);
    } catch {
      /* quota / disabled */
    }
  }

  get(sessionId: string): Entry | undefined {
    try {
      const raw = this.read(KEY_PREFIX + sessionId);
      const v = raw ? (JSON.parse(raw) as Partial<Entry>) : undefined;
      if (!v || !isMode(v.m)) return undefined;
      return { m: v.m, n: typeof v.n === 'number' && v.n >= 0 ? Math.floor(v.n) : 0 };
    } catch {
      return undefined;
    }
  }

  set(sessionId: string, e: Entry): void {
    this.write(KEY_PREFIX + sessionId, JSON.stringify(e));
  }

  /**
   * Read-increment-write of the segment counter, done right before a segment is sealed so the window in which two tabs
   * could read the same value is a few microseconds. `mode` is merged in: `session` (an error in any tab) wins.
   */
  takeSegment(sessionId: string, mode: StoredMode): number {
    const cur = this.get(sessionId);
    const n = cur?.n ?? 0;
    const m: StoredMode = cur?.m === 'session' || mode === 'session' ? 'session' : (cur?.m ?? mode);
    this.set(sessionId, { m, n: n + 1 });
    return n;
  }

  /** Remove the entries of every session except `keep` (old sessions never come back). */
  cleanup(keep: string): void {
    const keepKey = KEY_PREFIX + keep;
    for (const k of [...this.mem.keys()]) if (k !== keepKey) this.mem.delete(k);
    const s = this.storage;
    if (!s) return;
    try {
      const stale: string[] = [];
      for (let i = 0; i < s.length; i++) {
        const k = s.key(i);
        if (k && k.startsWith(KEY_PREFIX) && k !== keepKey) stale.push(k);
      }
      for (const k of stale) s.removeItem(k);
    } catch {
      /* ignore */
    }
  }
}
