import { useLayoutEffect, useSyncExternalStore } from 'react';
import { useLocation } from 'react-router';
import { useApp } from './context';

const REDUCE = '(prefers-reduced-motion: reduce)';
const subscribeReduce = (cb: () => void) => {
  const m = window.matchMedia(REDUCE);
  m.addEventListener('change', cb);
  return () => m.removeEventListener('change', cb);
};

/** Decorative motion (card reveal/ambient, liquid cursor) runs only when neither the OS nor the user asked to reduce it. */
export function useMotionAllowed(): boolean {
  const { motion } = useApp();
  const osReduce = useSyncExternalStore(subscribeReduce, () => window.matchMedia(REDUCE).matches, () => true);
  return motion === 'full' && !osReduce;
}

/**
 * Cards in `.content` fade/slide in once as they scroll into view and get an ambient offset so their idle
 * pulse is staggered. Cards already on screen at navigation settle immediately (no flash on first paint).
 */
export function useCardReveal(enabled: boolean) {
  const location = useLocation();

  useLayoutEffect(() => {
    const content = document.querySelector<HTMLElement>('.content');
    if (!content || !enabled) return;

    const seen = new WeakSet<Element>();
    let revealed = 0;
    let seenCount = 0;
    const observer = new IntersectionObserver((entries) => {
      for (const entry of entries) {
        const card = entry.target as HTMLElement;
        if (!entry.isIntersecting) {
          delete card.dataset.inView;
          card.dataset.revealFrom = card.getBoundingClientRect().bottom <= 0 ? 'above' : 'below';
          card.dataset.reveal = 'pending';
          continue;
        }
        card.dataset.inView = 'true';
        if (card.dataset.reveal === 'pending') {
          card.style.setProperty('--reveal-delay', `${(revealed % 3) * 75}ms`);
          revealed += 1;
          card.dataset.reveal = 'visible';
        }
      }
    }, { threshold: 0 });

    let frame = 0;
    const collect = () => {
      frame = 0;
      for (const card of content.querySelectorAll<HTMLElement>('.card')) {
        if (seen.has(card)) continue;
        seen.add(card);
        const bounds = card.getBoundingClientRect();
        card.style.setProperty('--ambient-offset', `${-(seenCount++ % 5) * 1.1}s`);
        card.dataset.revealFrom = 'below';
        card.dataset.reveal = bounds.top < window.innerHeight * 0.85 || bounds.bottom < 0 ? 'settled' : 'pending';
        observer.observe(card);
      }
    };
    const changes = new MutationObserver(() => { if (!frame) frame = requestAnimationFrame(collect); });
    collect();
    changes.observe(content, { childList: true, subtree: true });
    return () => {
      changes.disconnect();
      observer.disconnect();
      if (frame) cancelAnimationFrame(frame);
      for (const card of content.querySelectorAll<HTMLElement>('.card')) {
        delete card.dataset.reveal; delete card.dataset.revealFrom; delete card.dataset.inView;
      }
    };
  }, [location.pathname, enabled]);
}
