import { safe, truncate, type Client, type ViewRef } from '@trooper/core';
import type { Activity } from './activity';

export const RAGE_CLICKS = 3;
export const ACTION_WINDOW_MS = 1000;

const INTERACTIVE =
  'button,a,[role=button],[role=link],[role=menuitem],[role=tab],[role=checkbox],[role=switch],input,select,textarea,summary,label,[data-trooper-name]';
/** Clicks on these legitimately produce no DOM change (focus, typing, native pickers): never "dead". */
const NO_DEAD = 'input,textarea,select,label,option,[contenteditable],[contenteditable=""],[contenteditable=true]';

interface Pending {
  el: Element;
  ts: number;
  count: number;
  active: boolean;
  deadEligible: boolean;
  view: ViewRef | undefined;
  timer: ReturnType<typeof setTimeout>;
}

export function actionName(el: Element): string {
  const attr = (n: string) => el.getAttribute(n)?.trim();
  let name = attr('data-trooper-name') || attr('aria-label');
  if (!name && el instanceof HTMLInputElement && /^(button|submit|reset)$/.test(el.type)) name = el.value;
  if (!name && !(el instanceof HTMLInputElement || el instanceof HTMLTextAreaElement || el instanceof HTMLSelectElement)) {
    name = (el.textContent || '').replace(/\s+/g, ' ').trim();
  }
  name = name || attr('title') || attr('alt') || attr('name') || el.tagName.toLowerCase();
  return truncate(name, 50);
}

const NOISY_CLASS = /\d{3,}|^css-|^sc-|^jsx-|^_|^svelte-|^ng-/;

/** Short, mostly stable CSS-ish selector: up to 3 ancestors, stops at an id. */
export function selectorOf(el: Element): string {
  const parts: string[] = [];
  let e: Element | null = el;
  for (let i = 0; e && i < 3; e = e.parentElement, i++) {
    let s = e.tagName.toLowerCase();
    if (e.id && !/\d{3,}/.test(e.id)) {
      parts.unshift(`${s}#${e.id}`);
      break;
    }
    const cls = Array.from(e.classList)
      .filter((c) => !NOISY_CLASS.test(c))
      .slice(0, 2);
    if (cls.length) s += '.' + cls.join('.');
    parts.unshift(s);
  }
  return truncate(parts.join(' > '), 120);
}

/**
 * Clicks → ACTION events, evaluated over a 1 s window:
 * - rage: ≥3 clicks on the same element within 1 s (merged into one action)
 * - dead: no DOM mutation / navigation / request within 1 s after the click
 */
export class Actions {
  private pending = new Map<Element, Pending>();
  private unsub: (() => void) | undefined;

  constructor(
    private readonly client: Client,
    private readonly activity: Activity,
  ) {}

  start(): void {
    this.unsub = this.activity.subscribe(() => this.pending.forEach((p) => (p.active = true)));
    addEventListener('click', this.onClick, true);
  }

  stop(): void {
    removeEventListener('click', this.onClick, true);
    this.unsub?.();
    this.flushAll(true);
  }

  private onClick = (ev: MouseEvent): void => {
    safe('actions.click', () => {
      const raw = ev.target;
      if (!(raw instanceof Element)) return;
      const el = raw.closest(INTERACTIVE) ?? raw;
      const now = Date.now();
      const p = this.pending.get(el);
      if (p && now - p.ts <= ACTION_WINDOW_MS) {
        p.count++;
        return;
      }
      if (p) this.finish(p, false);
      const sel = typeof getSelection === 'function' ? getSelection()?.toString() : '';
      const np: Pending = {
        el,
        ts: now,
        count: 1,
        active: false,
        deadEligible: !el.closest(NO_DEAD) && !sel,
        view: this.client.view,
        timer: setTimeout(() => this.finish(np, false), ACTION_WINDOW_MS),
      };
      this.pending.set(el, np);
    });
  };

  /** Emit pending actions now (page hide). Navigation away means the clicks were not dead. */
  flushAll(navigated: boolean): void {
    this.pending.forEach((p) => this.finish(p, navigated));
  }

  private finish(p: Pending, navigated: boolean): void {
    clearTimeout(p.timer);
    if (this.pending.get(p.el) === p) this.pending.delete(p.el);
    const v = this.client.view?.id === p.view?.id ? this.client.view : p.view;
    this.client.capture({
      type: 'ACTION',
      ts_ms: p.ts,
      view_id: v?.id,
      view_name: v?.name,
      action: {
        name: actionName(p.el),
        target: selectorOf(p.el),
        rage: p.count >= RAGE_CLICKS,
        dead: p.deadEligible && !p.active && !navigated,
      },
      attributes: p.count > 1 ? { click_count: String(p.count) } : undefined,
    });
  }
}
