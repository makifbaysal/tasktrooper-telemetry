import { useMemo, useState } from 'react';
import { clock, dateTime } from '@/lib/format';
import { Legend } from './Legend';
import { niceTicks, seriesColor, useWidth } from './scale';

export interface Series {
  id: string;
  label: string;
  data: number[];
  /** categorical slot (1..8) — fixed per entity */
  slot?: number;
  color?: string;
}

interface Props {
  timestamps: number[];
  series: Series[];
  height?: number;
  format?: (v: number) => string;
  area?: boolean;
  threshold?: { value: number; label: string };
  /** vertical dashed markers (deploys, migrations); `detail` shows in the tooltip when hovering the marker's point */
  events?: { ts: number; label: string; detail?: string }[];
  yMax?: number;
  /** x-axis / tooltip label; defaults to wall-clock time */
  formatX?: (t: number) => string;
}

const PAD = { top: 8, right: 8, bottom: 22, left: 44 };

export function TimeSeries({ timestamps, series, height = 200, format = String, area = false, threshold, events, yMax, formatX }: Props) {
  const [ref, width] = useWidth<HTMLDivElement>();
  const [hover, setHover] = useState<number | null>(null);
  const colored = series.map((s, i) => ({ ...s, stroke: s.color ?? seriesColor(s.slot ?? i + 1) }));

  const { ticks, max } = useMemo(() => {
    const dataMax = Math.max(...series.flatMap((s) => s.data), threshold?.value ?? 0, 0);
    const t = niceTicks(yMax ?? dataMax * 1.08);
    return { ticks: t, max: t[t.length - 1] };
  }, [series, threshold, yMax]);

  const iw = Math.max(0, width - PAD.left - PAD.right);
  const ih = height - PAD.top - PAD.bottom;
  const n = timestamps.length;
  const x = (i: number) => PAD.left + (n <= 1 ? 0 : (i / (n - 1)) * iw);
  const y = (v: number) => PAD.top + ih - (v / max) * ih;
  const spanMs = timestamps[n - 1] - timestamps[0];
  const fmtT = (t: number) => formatX ? formatX(t) : (spanMs > 2 * 86400_000 ? dateTime(t) : clock(t, spanMs < 5 * 60_000));
  // one tick per ~110px so labels never collide
  // (and never more ticks than points, or short series repeat a label on top of itself)
  const tickCount = Math.max(1, Math.min(5, n, Math.floor(iw / 110) + 1));
  const xTickIdx = tickCount === 1 ? [0] : Array.from({ length: tickCount }, (_, k) => Math.round((k * (n - 1)) / (tickCount - 1)));

  const path = (d: number[]) => d.map((v, i) => `${i ? 'L' : 'M'}${x(i).toFixed(1)},${y(v).toFixed(1)}`).join('');

  function onMove(e: React.PointerEvent<SVGRectElement>) {
    const rect = (e.currentTarget as SVGRectElement).getBoundingClientRect();
    const rel = (e.clientX - rect.left) / rect.width;
    setHover(Math.max(0, Math.min(n - 1, Math.round(rel * (n - 1)))));
  }

  const tipLeft = hover !== null ? x(hover) : 0;
  const flip = tipLeft > width * 0.6;

  return (
    <div>
      <Legend items={colored.map((s) => ({ label: s.label, color: s.stroke }))} />
      <div ref={ref} className="chart" style={{ height }}>
        {width > 0 && (
          <svg width={width} height={height} role="img" aria-label={series.map((s) => s.label).join(', ')}>
            {ticks.map((t) => (
              <g key={t}>
                <line className={t === 0 ? 'chart__baseline' : 'chart__grid'} x1={PAD.left} x2={width - PAD.right} y1={y(t)} y2={y(t)} />
                <text className="chart__tick" x={PAD.left - 8} y={y(t)} dy="0.32em" textAnchor="end">{format(t)}</text>
              </g>
            ))}
            {xTickIdx.map((i, k) => (
              <text key={k} className="chart__tick" x={x(i)} y={height - 6} textAnchor={k === 0 ? 'start' : k === tickCount - 1 ? 'end' : 'middle'}>
                {fmtT(timestamps[i])}
              </text>
            ))}
            {events?.map((ev) => {
              const i = timestamps.findIndex((t) => t >= ev.ts);
              if (i < 0) return null;
              return (
                <g key={ev.ts}>
                  {ev.detail && <title>{`${ev.label} — ${ev.detail}`}</title>}
                  <line className="chart__event" x1={x(i)} x2={x(i)} y1={PAD.top} y2={PAD.top + ih} />
                  <text className="chart__tick" x={x(i) > width * 0.6 ? x(i) - 4 : x(i) + 4} y={PAD.top + 10} textAnchor={x(i) > width * 0.6 ? 'end' : 'start'}>{ev.label}</text>
                </g>
              );
            })}
            {area && colored.map((s) => (
              <path key={`a-${s.id}`} className="chart__area" fill={s.stroke}
                d={`${path(s.data)}L${x(n - 1)},${y(0)}L${x(0)},${y(0)}Z`} />
            ))}
            {colored.map((s) => (
              <path key={s.id} className="chart__line" stroke={s.stroke} d={path(s.data)} />
            ))}
            {threshold && (
              <g>
                <line className="chart__threshold" x1={PAD.left} x2={width - PAD.right} y1={y(threshold.value)} y2={y(threshold.value)} />
                <text className="chart__threshold-label" x={width - PAD.right} y={y(threshold.value) - 4} textAnchor="end">{threshold.label}</text>
              </g>
            )}
            {hover !== null && (
              <g>
                <line className="chart__crosshair" x1={x(hover)} x2={x(hover)} y1={PAD.top} y2={PAD.top + ih} />
                {colored.map((s) => (
                  <circle key={s.id} className="chart__marker" cx={x(hover)} cy={y(s.data[hover])} r={4} fill={s.stroke} />
                ))}
              </g>
            )}
            <rect x={PAD.left} y={0} width={iw} height={height} fill="transparent"
              onPointerMove={onMove} onPointerLeave={() => setHover(null)} />
          </svg>
        )}
        {hover !== null && (
          <div className="chart__tooltip" style={{ top: PAD.top, left: flip ? undefined : tipLeft + 12, right: flip ? width - tipLeft + 12 : undefined }}>
            <div className="chart__tooltip-time">{fmtT(timestamps[hover])}</div>
            {events?.filter((ev) => { const i = timestamps.findIndex((t) => t >= ev.ts); return i >= 0 && Math.abs(i - hover) <= 1; }).map((ev) => (
              <div key={ev.ts} style={{ maxWidth: 'calc(var(--space-6) * 12)', marginBottom: 'var(--space-1)' }}>
                <strong>{ev.label}</strong>{ev.detail && <span className="muted"> · {ev.detail}</span>}
              </div>
            ))}
            {colored.map((s) => (
              <div key={s.id} className="chart__tooltip-row">
                <span className="legend__key" style={{ background: s.stroke }} />
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
