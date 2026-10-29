import { useEffect, useRef } from 'react';

const SIZE = 18;
const INTERACTIVE = 'button, a, select, [role="button"]';
const TEXT_INPUT = 'input, textarea, [contenteditable="true"]';

export function LiquidCursor() {
  const cursorRef = useRef<HTMLDivElement>(null);

  useEffect(() => {
    const cursor = cursorRef.current;
    const finePointer = window.matchMedia('(hover: hover) and (pointer: fine)');
    const reducedMotion = window.matchMedia('(prefers-reduced-motion: reduce)');
    if (!cursor || !finePointer.matches || reducedMotion.matches) return;

    let pointerX = 0;
    let pointerY = 0;
    let x = 0;
    let y = 0;
    let width = SIZE;
    let height = SIZE;
    let nextX = 0;
    let nextY = 0;
    let nextWidth = SIZE;
    let nextHeight = SIZE;
    let frame = 0;
    let initialized = false;
    let idle = false;
    let idleTimer = 0;
    let lastMotionX = 0;
    let lastMotionY = 0;
    let hovered: Element | null = null;

    const armIdle = () => {
      if (idleTimer) window.clearTimeout(idleTimer);
      idleTimer = window.setTimeout(() => {
        idle = true;
        cursor.dataset.idle = 'true';
        cursor.style.opacity = '0';
      }, 1600);
    };

    const tick = () => {
      x += (nextX - x) * 0.2;
      y += (nextY - y) * 0.2;
      width += (nextWidth - width) * 0.2;
      height += (nextHeight - height) * 0.2;
      const dx = nextX - x;
      const dy = nextY - y;
      const speed = Math.hypot(dx, dy);
      if (!hovered && speed > 0.6) {
        const angle = Math.atan2(dy, dx);
        const stretch = 1 + Math.min(speed * 0.012, 0.55);
        cursor.style.transform = `translate3d(${x}px, ${y}px, 0) rotate(${angle}rad) scale(${stretch}, ${1 / stretch}) rotate(${-angle}rad)`;
        cursor.dataset.moving = 'true';
      } else {
        cursor.style.transform = `translate3d(${x}px, ${y}px, 0)`;
        delete cursor.dataset.moving;
      }
      cursor.style.width = `${width}px`;
      cursor.style.height = `${height}px`;
      if (Math.max(Math.abs(nextX - x), Math.abs(nextY - y), Math.abs(nextWidth - width), Math.abs(nextHeight - height)) > 0.15) {
        frame = requestAnimationFrame(tick);
      } else {
        frame = 0;
      }
    };
    const schedule = () => { if (!frame) frame = requestAnimationFrame(tick); };

    const update = () => {
      const element = document.elementFromPoint(pointerX, pointerY);
      const editing = element?.closest(TEXT_INPUT);
      cursor.style.opacity = editing || idle ? '0' : '1';
      const control = editing ? null : element?.closest(INTERACTIVE);
      const enabled = control && !control.matches(':disabled, [aria-disabled="true"]') ? control : null;

      if (enabled) {
        const bounds = enabled.getBoundingClientRect();
        nextX = bounds.left;
        nextY = bounds.top;
        nextWidth = bounds.width;
        nextHeight = bounds.height;
        if (hovered !== enabled) {
          hovered = enabled;
          const styles = getComputedStyle(enabled);
          const background = styles.backgroundColor;
          const transparent = background === 'transparent' || background === 'rgba(0, 0, 0, 0)' || background === 'rgb(0 0 0 / 0)';
          cursor.style.setProperty('--liquid-color', transparent ? 'var(--bg-hover)' : background);
          cursor.style.borderRadius = styles.borderRadius || 'var(--radius-sm)';
          cursor.dataset.morphed = 'true';
        }
      } else {
        hovered = null;
        nextX = pointerX - SIZE / 2;
        nextY = pointerY - SIZE / 2;
        nextWidth = SIZE;
        nextHeight = SIZE;
        cursor.style.borderRadius = '';
        delete cursor.dataset.morphed;
      }
      schedule();
    };

    const move = (event: PointerEvent) => {
      if (event.pointerType !== 'mouse' && event.pointerType !== 'pen') return;
      pointerX = event.clientX;
      pointerY = event.clientY;
      if (!initialized) {
        x = pointerX - SIZE / 2;
        y = pointerY - SIZE / 2;
        initialized = true;
        lastMotionX = pointerX;
        lastMotionY = pointerY;
        idle = false;
        delete cursor.dataset.idle;
        armIdle();
      } else if (Math.hypot(pointerX - lastMotionX, pointerY - lastMotionY) > 1.5) {
        lastMotionX = pointerX;
        lastMotionY = pointerY;
        idle = false;
        delete cursor.dataset.idle;
        armIdle();
      }
      update();
    };
    const leave = (event: Event) => {
      if (event instanceof MouseEvent && event.relatedTarget) return;
      cursor.style.opacity = '0';
      initialized = false;
      if (idleTimer) window.clearTimeout(idleTimer);
      idleTimer = 0;
    };
    const scroll = () => { if (initialized) update(); };
    const click = (event: PointerEvent) => {
      if ((event.pointerType !== 'mouse' && event.pointerType !== 'pen') || event.button !== 0) return;
      move(event);
      if (cursor.style.opacity === '0') return;
      delete cursor.dataset.clicking;
      void cursor.offsetWidth;
      cursor.dataset.clicking = 'true';
    };
    const finishClick = (event: AnimationEvent) => {
      if (event.animationName === 'liquid-click') delete cursor.dataset.clicking;
    };

    window.addEventListener('pointermove', move, { passive: true });
    window.addEventListener('pointerdown', click);
    window.addEventListener('mouseout', leave);
    window.addEventListener('scroll', scroll, { passive: true, capture: true });
    window.addEventListener('resize', scroll);
    window.addEventListener('blur', leave);
    cursor.addEventListener('animationend', finishClick);
    return () => {
      window.removeEventListener('pointermove', move);
      window.removeEventListener('pointerdown', click);
      window.removeEventListener('mouseout', leave);
      window.removeEventListener('scroll', scroll, true);
      window.removeEventListener('resize', scroll);
      window.removeEventListener('blur', leave);
      cursor.removeEventListener('animationend', finishClick);
      if (frame) cancelAnimationFrame(frame);
      if (idleTimer) window.clearTimeout(idleTimer);
    };
  }, []);

  return <div ref={cursorRef} className="liquid-cursor" aria-hidden="true" />;
}
