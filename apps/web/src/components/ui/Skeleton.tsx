import type { CSSProperties } from 'react';

/**
 * Loading placeholders: shimmering blocks shaped like the content that is coming, instead of a spinner and a
 * "Loading…" label. Screen readers get one polite "Loading" status per placeholder group.
 */
export function Skeleton({ w, h, radius, className, style }: {
  w?: CSSProperties['width']; h?: CSSProperties['height']; radius?: 'sm' | 'md' | 'full'; className?: string; style?: CSSProperties;
}) {
  return <span className={`skel${radius ? ` skel--${radius}` : ''}${className ? ` ${className}` : ''}`} style={{ width: w, height: h, ...style }} aria-hidden />;
}

/** A few lines of text (last one shorter). */
export function SkeletonText({ lines = 3, label = 'Loading' }: { lines?: number; label?: string }) {
  return (
    <div className="skel-group" role="status" aria-label={label}>
      {Array.from({ length: lines }, (_, i) => <Skeleton key={i} w={i === lines - 1 && lines > 1 ? '60%' : '100%'} h="var(--space-3)" />)}
    </div>
  );
}

/** Table-like rows: a leading block and a few columns. */
export function SkeletonRows({ rows = 6, label = 'Loading' }: { rows?: number; label?: string }) {
  return (
    <div className="skel-rows" role="status" aria-label={label}>
      {Array.from({ length: rows }, (_, i) => (
        <div key={i} className="skel-rows__row" style={{ ['--i' as string]: i }}>
          <Skeleton w="var(--space-4)" h="var(--space-4)" radius="sm" />
          <Skeleton w={`${38 + ((i * 17) % 30)}%`} h="var(--space-3)" />
          <Skeleton w="12%" h="var(--space-3)" className="skel-rows__end" />
          <Skeleton w="10%" h="var(--space-3)" />
        </div>
      ))}
    </div>
  );
}

/** Page body: header, metric tiles and a table. Used while a screen's data loads. */
export function SkeletonPage({ label = 'Loading' }: { label?: string }) {
  return (
    <div className="skel-page" role="status" aria-label={label}>
      <div className="skel-group skel-page__head">
        <Skeleton w="var(--space-32)" h="var(--space-6)" />
        <Skeleton w="min(28rem, 70%)" h="var(--space-3)" />
      </div>
      <div className="skel-page__tiles">
        {[0, 1, 2].map((i) => (
          <div key={i} className="skel-page__tile">
            <Skeleton w="40%" h="var(--space-3)" />
            <Skeleton w="30%" h="var(--space-8)" />
            <Skeleton w="100%" h="var(--space-16)" radius="md" />
          </div>
        ))}
      </div>
      <SkeletonRows rows={6} label={label} />
    </div>
  );
}
