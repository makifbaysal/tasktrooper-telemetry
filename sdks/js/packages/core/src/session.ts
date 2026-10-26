import { uuid } from './ids';
import { safe } from './util';

export interface SessionState {
  id: string;
  /** Session start (unix ms). */
  start: number;
  /** Last activity (unix ms). */
  last: number;
}

/** Synchronous persistence (browser: localStorage; shared between tabs). Optional. */
export interface SessionStore {
  load(): SessionState | undefined;
  save(s: SessionState): void;
}

export const SESSION_IDLE_MS = 30 * 60 * 1000;
export const SESSION_MAX_MS = 4 * 60 * 60 * 1000;

export interface SessionTouch {
  session: SessionState;
  /** true when a new session was created by this touch. */
  isNew: boolean;
}

/** Session rotated after 30 min inactivity or 4 h total (rum.proto `session_id`). */
export class SessionManager {
  private state: SessionState | undefined;
  private lastSaved = 0;

  constructor(
    private readonly store?: SessionStore,
    private readonly idleMs = SESSION_IDLE_MS,
    private readonly maxMs = SESSION_MAX_MS,
  ) {}

  private expired(s: SessionState, t: number): boolean {
    return t - s.last > this.idleMs || t - s.start > this.maxMs;
  }

  /** Current session without registering activity (may be expired). */
  peek(): SessionState | undefined {
    return this.state;
  }

  /** Register activity at time `t` and return the (possibly new) session. */
  touch(t: number): SessionTouch {
    let s = this.state;
    let isNew = false;
    if (!s || this.expired(s, t)) {
      // Another tab may have extended or created a session.
      const stored = this.store ? safe('session.load', () => this.store!.load()) : undefined;
      if (stored && typeof stored.id === 'string' && stored.id && !this.expired(stored, t)) {
        // Continue a session from a previous page load or another tab — not a SESSION_START.
        s = { ...stored };
      } else {
        s = { id: uuid(), start: t, last: t };
        isNew = true;
      }
    }
    s.last = Math.max(s.last, t);
    this.state = s;
    if (this.store && (isNew || t - this.lastSaved > 5000)) {
      this.lastSaved = t;
      const st = s;
      safe('session.save', () => this.store!.save(st));
    }
    return { session: s, isNew };
  }

  /** Continue an externally owned session (e.g. the native SDK's in a hybrid app). */
  adopt(id: string, start: number, t: number): SessionState {
    const s = { id, start: Math.min(start, t), last: t };
    this.state = s;
    if (this.store) safe('session.save', () => this.store!.save(s));
    return s;
  }

  /** Force a new session on the next touch (e.g. after `clearUser()` / logout). */
  reset(): void {
    this.state = undefined;
    if (this.store) safe('session.save', () => this.store!.save({ id: '', start: 0, last: 0 }));
  }
}
