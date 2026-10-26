import { useState } from 'react';
import { useWidth } from './scale';

export interface FlowNode {
  id: string;
  label: string;
  sub?: string;
  value: number;
  display: string;
  color: string;
}

interface Props {
  left: FlowNode[];
  center: { label: string; sub?: string };
  right: FlowNode[];
  leftTitle?: string;
  rightTitle?: string;
}

const ROW = 52;
const HEAD = 22;

/**
 * Sources → service → destinations. Band thickness ∝ volume (shared scale on both sides).
 * Bands carry the node's identity color at low opacity; text stays in ink tokens.
 */
export function Flow({ left, center, right, leftTitle = 'Incoming from', rightTitle = 'Outgoing to' }: Props) {
  const [ref, width] = useWidth<HTMLDivElement>();
  const [hover, setHover] = useState<string | null>(null);
  const n = Math.max(left.length, right.length, 3);
  const H = n * ROW;
  const nodeW = Math.min(230, Math.max(150, width * 0.27));
  const cW = Math.min(170, width * 0.18);
  const cx = width / 2 - cW / 2;
  const cTop = H * 0.12;
  const cH = H * 0.76;
  const sumL = left.reduce((a, x) => a + x.value, 0);
  const sumR = right.reduce((a, x) => a + x.value, 0);
  // fit both sides in the center box, and never let one band be taller than its node
  const maxNode = Math.max(...left.map((x) => x.value), ...right.map((x) => x.value), 1);
  const scale = Math.min((cH * 0.8) / Math.max(sumL, sumR, 1), 34 / maxNode);
  const thick = (v: number) => Math.max(2, v * scale);

  const y = (i: number, len: number) => (i + 0.5) * (H / len);

  function bands(nodes: FlowNode[], side: 'l' | 'r') {
    const total = nodes.reduce((a, x) => a + thick(x.value), 0);
    let off = cTop + (cH - total) / 2;
    return nodes.map((nd, i) => {
      const t = thick(nd.value);
      const yc = off + t / 2;
      off += t;
      const yn = y(i, nodes.length);
      const x1 = side === 'l' ? nodeW : cx + cW;
      const x2 = side === 'l' ? cx : width - nodeW;
      const [ya, yb] = side === 'l' ? [yn, yc] : [yc, yn];
      const mx = (x1 + x2) / 2;
      const dim = hover !== null && hover !== nd.id;
      return (
        <path key={`${side}-${nd.id}`} d={`M${x1},${ya}C${mx},${ya} ${mx},${yb} ${x2},${yb}`}
          fill="none" stroke={nd.color} strokeWidth={t} strokeOpacity={dim ? 0.1 : hover === nd.id ? 0.55 : 0.3}
          onPointerEnter={() => setHover(nd.id)} onPointerLeave={() => setHover(null)}>
          <title>{`${nd.label}: ${nd.display}`}</title>
        </path>
      );
    });
  }

  function nodes(list: FlowNode[], side: 'l' | 'r') {
    const x = side === 'l' ? 0 : width - nodeW;
    return list.map((nd, i) => {
      const yc = y(i, list.length);
      const dim = hover !== null && hover !== nd.id;
      return (
        <g key={`${side}n-${nd.id}`} transform={`translate(${x},${yc - 20})`} opacity={dim ? 0.45 : 1}
          onPointerEnter={() => setHover(nd.id)} onPointerLeave={() => setHover(null)} style={{ cursor: 'default' }}>
          <rect width={nodeW} height={40} rx={8} className="flow__node" />
          <rect x={side === 'l' ? nodeW - 4 : 0} y={6} width={4} height={28} rx={2} fill={nd.color} />
          <text x={12} y={17} className="flow__label">{clip(nd.label, nodeW)}</text>
          {nd.sub && <text x={12} y={31} className="flow__sub">{clip(nd.sub, nodeW)}</text>}
          <text x={nodeW - 12} y={17} textAnchor="end" className="flow__value">{nd.display}</text>
        </g>
      );
    });
  }

  return (
    <div ref={ref} className="chart" style={{ height: H + HEAD }}>
      {width > 0 && (
        <svg width={width} height={H + HEAD} role="img" aria-label={`${leftTitle}: ${left.map((l) => `${l.label} ${l.display}`).join(', ')}. ${rightTitle}: ${right.map((r) => `${r.label} ${r.display}`).join(', ')}`}>
          <text x={0} y={12} className="flow__title">{leftTitle}</text>
          <text x={width} y={12} textAnchor="end" className="flow__title">{rightTitle}</text>
          <g transform={`translate(0,${HEAD})`}>
            {bands(left, 'l')}
            {bands(right, 'r')}
            <rect x={cx} y={cTop} width={cW} height={cH} rx={10} className="flow__center" />
            <text x={cx + cW / 2} y={cTop + cH / 2 - 4} textAnchor="middle" className="flow__label">{center.label}</text>
            {center.sub && <text x={cx + cW / 2} y={cTop + cH / 2 + 14} textAnchor="middle" className="flow__sub">{center.sub}</text>}
            {nodes(left, 'l')}
            {nodes(right, 'r')}
          </g>
        </svg>
      )}
    </div>
  );
}

/** Rough clip so labels never overflow their node (≈6.5px per char at 12px). */
function clip(s: string, w: number) {
  const max = Math.floor((w - 90) / 6.5);
  return s.length > max ? `${s.slice(0, Math.max(4, max - 1))}…` : s;
}
