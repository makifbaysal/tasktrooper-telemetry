import type { ReactNode } from 'react';

export interface BarItem {
  id: string;
  label: ReactNode;
  value: number;
  display: string;
}

/** Ranked horizontal bars — single series, so one color and no legend. */
export function BarList({ items, color = 'var(--series-1)', onSelect }: { items: BarItem[]; color?: string; onSelect?: (id: string) => void }) {
  const max = Math.max(...items.map((i) => i.value), 1);
  return (
    <div className="barlist">
      {items.map((i) => (
        <div key={i.id} className={`barlist__row${onSelect ? ' barlist__row--clickable' : ''}`} onClick={onSelect ? () => onSelect(i.id) : undefined} title={`${typeof i.label === 'string' ? i.label : ''} ${i.display}`}>
          <span className="barlist__label truncate">{i.label}</span>
          <span className="barlist__value">{i.display}</span>
          <div className="barlist__track">
            <div className="barlist__bar" style={{ width: `${(i.value / max) * 100}%`, background: color }} />
          </div>
        </div>
      ))}
    </div>
  );
}

/** Part-to-whole in one row. Needs a legend (>=2 segments). */
export function PropBar({ segments, legend = true }: { segments: { label: string; value: number; color: string }[]; legend?: boolean }) {
  const total = segments.reduce((a, s) => a + s.value, 0) || 1;
  return (
    <div>
      <div className="propbar" role="img" aria-label={segments.map((s) => `${s.label} ${Math.round((s.value / total) * 100)}%`).join(', ')}>
        {segments.filter((s) => s.value > 0).map((s) => (
          <div key={s.label} className="propbar__seg" title={`${s.label}: ${((s.value / total) * 100).toFixed(1)}%`} style={{ flex: s.value, background: s.color }} />
        ))}
      </div>
      {legend && <div className="legend" style={{ marginTop: 8, marginBottom: 0 }}>
        {segments.map((s) => (
          <span key={s.label} className="legend__item">
            <span className="legend__swatch" style={{ background: s.color }} />
            {s.label} <span className="muted num">{((s.value / total) * 100).toFixed(1)}%</span>
          </span>
        ))}
      </div>}
    </div>
  );
}

export interface BreakdownSegment {
  id: string;
  label: string;
  value: number;
  color: string;
}

/**
 * Horizontal stacked bar on an absolute scale (`max` shared across rows),
 * so rows compare by total length and by composition. 2px surface gap between segments.
 */
export function BreakdownBar({ segments, max, format }: { segments: BreakdownSegment[]; max: number; format: (v: number) => string }) {
  const total = segments.reduce((a, s) => a + s.value, 0);
  const visible = segments.filter((s) => s.value > 0);
  return (
    <div className="breakdown" role="img" aria-label={visible.map((s) => `${s.label} ${format(s.value)}`).join(', ')}>
      <div className="breakdown__bar" style={{ width: `${(total / (max || 1)) * 100}%` }}>
        {visible.map((s) => (
          <div key={s.id} className="breakdown__seg" style={{ flex: s.value, background: s.color }}
            title={`${s.label}: ${format(s.value)} (${Math.round((s.value / total) * 100)}%)`} />
        ))}
      </div>
    </div>
  );
}
