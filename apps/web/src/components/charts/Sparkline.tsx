/** 12+ point trend. De-emphasis line, current point in accent. */
export function Sparkline({ data, width = 96, height = 28 }: { data: number[]; width?: number; height?: number }) {
  if (data.length < 2) return null;
  const min = Math.min(...data), max = Math.max(...data);
  const pad = 4;
  const x = (i: number) => pad + (i / (data.length - 1)) * (width - pad * 2);
  const y = (v: number) => pad + (height - pad * 2) * (1 - (max === min ? 0.5 : (v - min) / (max - min)));
  const d = data.map((v, i) => `${i ? 'L' : 'M'}${x(i).toFixed(1)},${y(v).toFixed(1)}`).join('');
  const last = data.length - 1;
  return (
    <svg width={width} height={height} aria-hidden style={{ overflow: 'visible', flex: 'none' }}>
      <path d={d} fill="none" stroke="var(--text-muted)" strokeWidth={1.5} strokeLinejoin="round" strokeLinecap="round" />
      <circle cx={x(last)} cy={y(data[last])} r={3} fill="var(--accent)" stroke="var(--bg-surface)" strokeWidth={2} />
    </svg>
  );
}
