export function compact(n: number, digits = 1): string {
  const abs = Math.abs(n);
  if (abs >= 1e9) return `${trim(n / 1e9, digits)}B`;
  if (abs >= 1e6) return `${trim(n / 1e6, digits)}M`;
  if (abs >= 1e3) return `${trim(n / 1e3, digits)}K`;
  return trim(n, abs < 10 && n % 1 !== 0 ? digits : 0);
}

function trim(n: number, digits: number): string {
  return Number(n.toFixed(digits)).toString();
}

export function int(n: number): string {
  return Math.round(n).toLocaleString('en-US');
}

/** ratio 0..1 → "1.24%" */
export function pct(ratio: number, digits = 2): string {
  const v = ratio * 100;
  if (v === 0) return '0%';
  if (v < 0.001) return '<0.001%';
  // more decimals for tiny rates so axis ticks never collapse to the same label
  const d = v >= 10 ? 1 : v >= 1 ? digits : v >= 0.1 ? Math.max(digits, 2) : 3;
  return `${trim(v, d)}%`;
}

export function ms(v: number): string {
  if (v >= 1000) return `${trim(v / 1000, v >= 10000 ? 0 : 2)} s`;
  if (v === 0) return '0 ms';
  if (v < 1) return `${trim(v * 1000, 0)} µs`;
  return v < 10 ? `${trim(v, 1)} ms` : `${Math.round(v)} ms`;
}

/** "45s", "8m", "8m 20s", "1h", "1h 5m", "2d 3h" — zero parts are dropped. */
export function duration(seconds: number): string {
  const total = Math.round(seconds);
  const d = Math.floor(total / 86400);
  const h = Math.floor((total % 86400) / 3600);
  const m = Math.floor((total % 3600) / 60);
  const s = total % 60;
  const join = (a: number, ua: string, b: number, ub: string) => (b ? `${a}${ua} ${b}${ub}` : `${a}${ua}`);
  if (d) return join(d, 'd', h, 'h');
  if (h) return join(h, 'h', m, 'm');
  if (m) return join(m, 'm', s, 's');
  return `${s}s`;
}

export function clock(ts: number, withSeconds = false): string {
  return new Date(ts).toLocaleTimeString('en-GB', {
    hour: '2-digit',
    minute: '2-digit',
    ...(withSeconds ? { second: '2-digit' } : {}),
  });
}

export function dateTime(ts: number): string {
  return new Date(ts).toLocaleString('en-GB', { month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit' });
}

export function ago(ts: number, now = Date.now()): string {
  const s = Math.max(0, Math.round((now - ts) / 1000));
  if (s < 60) return `${s}s ago`;
  const m = Math.round(s / 60);
  if (m < 60) return `${m}m ago`;
  const h = Math.round(m / 60);
  if (h < 48) return `${h}h ago`;
  return `${Math.round(h / 24)}d ago`;
}

/** Signed relative change, e.g. +12.4% */
export function change(curr: number, prev: number): { ratio: number; label: string } {
  const ratio = prev === 0 ? 0 : (curr - prev) / prev;
  const sign = ratio > 0 ? '+' : ratio < 0 ? '−' : '';
  return { ratio, label: `${sign}${trim(Math.abs(ratio) * 100, 1)}%` };
}
