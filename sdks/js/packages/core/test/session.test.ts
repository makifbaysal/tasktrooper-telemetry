import { describe, expect, it } from 'vitest';
import { SESSION_IDLE_MS, SESSION_MAX_MS, SessionManager, type SessionState } from '../src/session';

describe('SessionManager', () => {
  it('keeps the session while active and rotates after 30 min idle', () => {
    const s = new SessionManager();
    const a = s.touch(0);
    expect(a.isNew).toBe(true);
    expect(s.touch(SESSION_IDLE_MS - 1).session.id).toBe(a.session.id);
    const b = s.touch(2 * SESSION_IDLE_MS);
    expect(b.isNew).toBe(true);
    expect(b.session.id).not.toBe(a.session.id);
  });

  it('rotates after 4 h even when active', () => {
    const s = new SessionManager();
    const first = s.touch(0).session.id;
    let t = 0;
    while (t < SESSION_MAX_MS) {
      t += 10 * 60 * 1000;
      if (t <= SESSION_MAX_MS) expect(s.touch(t).session.id).toBe(first);
    }
    expect(s.touch(SESSION_MAX_MS + 1).isNew).toBe(true);
  });

  it('continues a persisted session (other tab / reload) without SESSION_START', () => {
    let saved: SessionState | undefined = { id: 'stored', start: 0, last: 1000 };
    const store = { load: () => saved, save: (x: SessionState) => void (saved = { ...x }) };
    const s = new SessionManager(store);
    const t = s.touch(2000);
    expect(t.isNew).toBe(false);
    expect(t.session.id).toBe('stored');
    expect(new SessionManager(store).touch(2000 + SESSION_IDLE_MS + 5000).isNew).toBe(true);
  });
});
