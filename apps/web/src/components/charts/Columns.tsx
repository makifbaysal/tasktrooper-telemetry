import { useMemo, useState } from 'react';
import { clock, dateTime } from '@/lib/format';
import { Legend } from './Legend';
import { niceTicks, useWidth } from './scale';

export interface ColumnSeries {
  id: string;
  label: string;
  data: number[];
  color: string;
}

/** Stacked columns over time — status codes, log volume by level. 2px surface gap between segments. */
export function Columns({ timestamps, series, height = 160, format = String }: {
  timestamps: number[]; series: ColumnSeries[]; height?: number; format?: (v: number) => string;
}) {
  const [ref, width] = useWidth<HTMLDivElement>();
  const [hover, setHover] = useState<number | null>(null);
  const PAD = { top: 8, right: 8, bottom: 22, left: 44 };
  const n = timestamps.length;
  const totals = useMemo(() => timestamps.map((_, i) => series.reduce((a, s) => a + s.data[i], 0)), [timestamps, series]);
  const ticks = niceTicks(Math.max(...totals) * 1.05);
  const max = ticks[ticks.length - 1];
  const iw = Math.max(0, width - PAD.left - PAD.right);
  const ih = height - PAD.top - PAD.bottom;
  const slot = iw / n;
  const bw = Math.min(24, Math.max(1, slot - 2));
  const y = (v: number) => PAD.top + ih - (v / max) * ih;
  const spanMs = timestamps[n - 1] - timestamps[0];
  const fmtT = (t: number) => (spanMs > 2 * 86400_000 ? dateTime(t) : clock(t));
  const GAP = 2;

  return (
    <div>
      <Legend kind="swatch" items={series.map((s) => ({ label: s.label, color: s.color }))} />
      <div ref={ref} className="chart" style={{ height }}>
        {width > 0 && (
          <svg width={width} height={height} onPointerLeave={() => setHover(null)}>
            {ticks.map((t) => (
              <g key={t}>
                <line className={t === 0 ? 'chart__baseline' : 'chart__grid'} x1={PAD.left} x2={width - PAD.right} y1={y(t)} y2={y(t)} />
                <text className="chart__tick" x={PAD.left - 8} y={y(t)} dy="0.32em" textAnchor="end">{format(t)}</text>
              </g>
            ))}
            {[0, Math.round((n - 1) / 2), n - 1].map((i, k) => (
              <text key={k} className="chart__tick" x={PAD.left + i * slot + slot / 2} y={height - 6} textAnchor={k === 0 ? 'start' : k === 2 ? 'end' : 'middle'}>{fmtT(timestamps[i])}</text>
            ))}
            {timestamps.map((_, i) => {
              const cx = PAD.left + i * slot + (slot - bw) / 2;
              let acc = 0;
              const visible = series.filter((s) => s.data[i] > 0);
              return (
                <g key={i} opacity={hover === null || hover === i ? 1 : 0.55} onPointerEnter={() => setHover(i)}>
                  <rect x={PAD.left + i * slot} y={PAD.top} width={slot} height={ih} fill="transparent" />
                  {visible.map((s, k) => {
                    const top = k === visible.length - 1;
                    const y0 = y(acc);
                    acc += s.data[i];
                    const y1 = y(acc);
                    const h = Math.max(0, y0 - y1 - (k > 0 ? GAP : 0));
                    const r = top ? Math.min(4, bw / 2, h) : 0;
                    const yb = y0 - (k > 0 ? GAP : 0);
                    // rounded data-end only on the top segment, square at the baseline
                    const d = `M${cx},${yb}V${yb - h + r}${r ? `Q${cx},${yb - h} ${cx + r},${yb - h}` : ''}H${cx + bw - r}${r ? `Q${cx + bw},${yb - h} ${cx + bw},${yb - h + r}` : ''}V${yb}Z`;
                    return <path key={s.id} d={d} fill={s.color} />;
                  })}
                </g>
              );
            })}
          </svg>
        )}
        {hover !== null && width > 0 && (
          <div className="chart__tooltip" style={{ top: PAD.top, ...(PAD.left + hover * slot > width * 0.6 ? { right: width - (PAD.left + hover * slot) + 8 } : { left: PAD.left + (hover + 1) * slot + 8 }) }}>
            <div className="chart__tooltip-time">{fmtT(timestamps[hover])}</div>
            {series.map((s) => (
              <div key={s.id} className="chart__tooltip-row">
                <span className="legend__swatch" style={{ background: s.color }} />
                <span>{s.label}</span>
                <span>{format(s.data[hover])}</span>
              </div>
            ))}
          </div>
        )}
      </div>
    </div>
  );
}
