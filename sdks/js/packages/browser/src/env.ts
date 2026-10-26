import type { Browser, Device, Os, SessionState, SessionStore } from '@trooper/core';

const re = (r: RegExp, s: string): string | undefined => r.exec(s)?.[1];

export function detectBrowser(ua: string): Browser {
  const tests: [string, RegExp][] = [
    ['Edge', /Edg(?:e|A|iOS)?\/(\d+)/],
    ['Opera', /OPR\/(\d+)/],
    ['Samsung Internet', /SamsungBrowser\/(\d+)/],
    ['Firefox', /(?:Firefox|FxiOS)\/(\d+)/],
    ['Chrome', /(?:Chrome|CriOS)\/(\d+)/],
    ['Safari', /Version\/(\d+(?:\.\d+)?).*Safari/],
  ];
  for (const [name, r] of tests) {
    const v = re(r, ua);
    if (v) return { name, version: v };
  }
  return { name: 'Other' };
}

export function detectOs(ua: string): Os {
  let v: string | undefined;
  if ((v = re(/Windows NT ([\d.]+)/, ua))) return { name: 'Windows', version: v };
  if ((v = re(/(?:iPhone|iPad|iPod).*? OS ([\d_]+)/, ua))) return { name: 'iOS', version: v.replace(/_/g, '.') };
  if ((v = re(/Android ([\d.]+)/, ua))) return { name: 'Android', version: v };
  if ((v = re(/Mac OS X ([\d_.]+)/, ua))) return { name: 'macOS', version: v.replace(/_/g, '.') };
  if (/CrOS/.test(ua)) return { name: 'ChromeOS' };
  if (/Linux/.test(ua)) return { name: 'Linux' };
  return { name: 'Other' };
}

export function detectDeviceType(ua: string): string {
  if (/iPad|Tablet|Android(?!.*Mobile)/i.test(ua)) return 'tablet';
  if (/Mobi|iPhone|iPod|Android/i.test(ua)) return 'mobile';
  if (/TV|SmartTV|CrKey/i.test(ua)) return 'tv';
  return 'desktop';
}

export function detectEnv(): { device: Device; os: Os; browser: Browser } {
  const ua = navigator.userAgent || '';
  return {
    device: {
      type: detectDeviceType(ua),
      locale: navigator.language,
      screen_width: typeof screen !== 'undefined' ? screen.width : undefined,
      screen_height: typeof screen !== 'undefined' ? screen.height : undefined,
    },
    os: detectOs(ua),
    browser: detectBrowser(ua),
  };
}

export function connection(): string {
  if (typeof navigator === 'undefined') return 'unknown';
  if (navigator.onLine === false) return 'offline';
  const c = (navigator as { connection?: { effectiveType?: string; type?: string } }).connection;
  if (c?.type === 'wifi' || c?.type === 'cellular') return c.type;
  return c?.effectiveType || 'unknown';
}

function ls(): Storage | undefined {
  try {
    return window.localStorage;
  } catch {
    return undefined;
  }
}

export function localGet(key: string): string | undefined {
  try {
    return ls()?.getItem(key) ?? undefined;
  } catch {
    return undefined;
  }
}

export function localSet(key: string, v: string): void {
  try {
    ls()?.setItem(key, v);
  } catch {
    /* quota / disabled */
  }
}

export const localSessionStore = (key = 'tt_session'): SessionStore => ({
  load() {
    const raw = localGet(key);
    if (!raw) return undefined;
    const s = JSON.parse(raw) as SessionState;
    return s && typeof s.id === 'string' && s.id ? s : undefined;
  },
  save(s) {
    localSet(key, JSON.stringify(s));
  },
});

export const perfNow = (): number =>
  typeof performance !== 'undefined' && typeof performance.now === 'function' ? performance.now() : Date.now();
