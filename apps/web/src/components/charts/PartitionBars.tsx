import { useState } from 'react';
import { compact } from '@/lib/format';

/**
 * Lag per partition as columns — one series, one color. The busiest partition gets a
 * direct label; hovering any column shows its value. Skew (one tall column) is the point.
 */
export function PartitionBars({ values, height = 72, label = (i: number) => `P${i}` }: { values: number[]; height?: number; label?: (i: number) => string }) {
  const [hover, setHover] = useState<number | null>(null);
  const max = Math.max(...values, 1);
  const top = values.indexOf(Math.max(...values));
  return (
    <div className="pbars" role="img" aria-label={`Lag per partition: ${values.map((v, i) => `${label(i)} ${compact(v)}`).join(', ')}`}>
      <div className="pbars__plot" style={{ height }}>
        {values.map((v, i) => (
          <div key={i} className="pbars__col" onMouseEnter={() => setHover(i)} onMouseLeave={() => setHover(null)}>
            {(i === top || i === hover) && <span className="pbars__value">{compact(v)}</span>}
            <div className={`pbars__bar${hover === i ? ' is-hover' : ''}`} style={{ height: `${Math.max(2, (v / max) * (height - 18))}px` }} />
          </div>
        ))}
      </div>
      <div className="pbars__axis">
        {values.map((_, i) => <span key={i}>{values.length <= 16 || i % 4 === 0 ? label(i) : ''}</span>)}
      </div>
    </div>
  );
}
