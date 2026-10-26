export function Legend({ items, kind = 'line' }: { items: { label: string; color: string }[]; kind?: 'line' | 'swatch' }) {
  if (items.length < 2) return null;
  return (
    <div className="legend">
      {items.map((i) => (
        <span key={i.label} className="legend__item">
          <span className={kind === 'line' ? 'legend__key' : 'legend__swatch'} style={{ background: i.color }} />
          {i.label}
        </span>
      ))}
    </div>
  );
}
