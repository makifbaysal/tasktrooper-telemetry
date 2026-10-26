import type { ReactNode } from 'react';
import { ArrowDownRight, ArrowUpRight, ChevronRight } from 'lucide-react';
import { change } from '@/lib/format';
import { Sparkline } from '@/components/charts/Sparkline';

/** Signed change vs previous period. Color = direction × whether up is good. */
export function Delta({ curr, prev, upIsGood = true, period = 'vs prev.' }: { curr: number; prev: number; upIsGood?: boolean; period?: string }) {
  const { ratio, label } = change(curr, prev);
  const flat = Math.abs(ratio) < 0.005;
  const good = ratio > 0 === upIsGood;
  const cls = flat ? 'delta--neutral' : good ? 'delta--good' : 'delta--bad';
  const Icon = ratio >= 0 ? ArrowUpRight : ArrowDownRight;
  return (
    <span className={`delta ${cls}`}>
      {!flat && <Icon size={14} aria-hidden />}
      {flat ? 'No change' : label}
      {period && <span className="delta__period">{period}</span>}
    </span>
  );
}

interface Props {
  label: ReactNode;
  value: ReactNode;
  unit?: string;
  /** current & previous raw values for delta */
  curr?: number;
  prev?: number;
  /** is an increase good? (throughput: yes, error rate: no) */
  upIsGood?: boolean;
  period?: string;
  trend?: number[];
  onClick?: () => void;
}

/** Compact number. Use when the metric has NO chart elsewhere on the page; otherwise use MetricCard. */
export function StatTile({ label, value, unit, curr, prev, upIsGood = true, period = 'vs prev.', trend, onClick }: Props) {
  return (
    <div className={`card stat${onClick ? ' card--interactive' : ''}`} onClick={onClick}>
      <div className="stat__label">{label}</div>
      <div className="stat__value">
        {value}
        {unit && <span className="stat__unit">{unit}</span>}
      </div>
      <div className="stat__foot">
        {curr !== undefined && prev !== undefined ? <Delta curr={curr} prev={prev} upIsGood={upIsGood} period={period} /> : <span />}
        {trend && <Sparkline data={trend} width={88} height={28} />}
      </div>
    </div>
  );
}

interface MetricCardProps {
  label: ReactNode;
  value: ReactNode;
  unit?: string;
  curr?: number;
  prev?: number;
  upIsGood?: boolean;
  period?: string;
  /** secondary facts under the value, e.g. "p50 101 ms · p99 504 ms" */
  hint?: ReactNode;
  /** status badge etc. next to the label */
  badge?: ReactNode;
  actions?: ReactNode;
  onClick?: () => void;
  className?: string;
  /** the chart — the metric's one and only visualization on the page */
  children?: ReactNode;
}

/**
 * Value + delta + chart in ONE card. A metric is shown once per page:
 * never a StatTile and a separate chart card for the same number.
 */
export function MetricCard({ label, value, unit, curr, prev, upIsGood = true, period, hint, badge, actions, onClick, className, children }: MetricCardProps) {
  return (
    <section className={['card metric', onClick && 'card--interactive', className].filter(Boolean).join(' ')} onClick={onClick}>
      <header className="metric__head">
        <div className="metric__label">{label}{badge}</div>
        {(actions || onClick) && <div className="card__actions">{actions}{onClick && !actions && <ChevronRight size={16} className="muted" aria-hidden />}</div>}
      </header>
      <div className="metric__value-row">
        <span className="metric__value">{value}{unit && <span className="stat__unit">{unit}</span>}</span>
        {curr !== undefined && prev !== undefined && <Delta curr={curr} prev={prev} upIsGood={upIsGood} period={period} />}
      </div>
      {hint && <div className="metric__hint">{hint}</div>}
      {children && <div className="metric__chart">{children}</div>}
    </section>
  );
}
