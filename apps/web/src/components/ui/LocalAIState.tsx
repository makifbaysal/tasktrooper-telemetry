import { useEffect, useState } from 'react';
import { Button } from './Button';

export function LocalAIState({ busy, error, onRetry }: { busy: boolean; error: string; onRetry: () => void }) {
  const [slow, setSlow] = useState(false);
  useEffect(() => {
    if (!busy) return;
    const timer = window.setTimeout(() => setSlow(true), 3000);
    return () => { window.clearTimeout(timer); setSlow(false); };
  }, [busy]);
  if (busy) return <p role="status" className="secondary" style={{ margin: 0 }}>{slow ? 'Still working on this draft…' : 'Working on this draft…'}</p>;
  if (error) return <div className="row" style={{ gap: 'var(--space-2)', flexWrap: 'wrap' }} role="alert">
    <span className="secondary">{error}</span><Button size="sm" onClick={onRetry}>Retry</Button>
  </div>;
  return null;
}
