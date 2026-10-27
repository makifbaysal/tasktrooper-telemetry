/** Privacy settings → rrweb record options (contracts.md §23 "Gizlilik"). */

export interface PrivacyOptions {
  maskAllText: boolean;
  maskAllInputs: boolean;
  blockSelector: string;
  maskSelector: string;
  unmaskSelector: string;
}

/** Keeps length and whitespace: "Jane Doe" → "**** ***". */
export const maskString = (s: string): string => s.replace(/\S/g, '*');

function closest(el: Element | null | undefined, sel: string): Element | null {
  if (!el || !sel || typeof el.closest !== 'function') return null;
  try {
    return el.closest(sel);
  } catch {
    return null; // invalid selector
  }
}

/**
 * Whether `el` is explicitly unmasked: the nearest of `[unmask]` / `[mask]` ancestors wins
 * (`<div data-trooper-unmask><span data-trooper-mask>` → masked).
 */
export function isUnmasked(el: Element | null | undefined, p: PrivacyOptions): boolean {
  const u = closest(el, p.unmaskSelector);
  if (!u) return false;
  const m = closest(el, p.maskSelector);
  return !m || (m !== u && m.contains(u));
}

const isPassword = (el: Element | null | undefined): boolean => {
  if (!el) return false;
  const type = (el.getAttribute?.('type') || '').toLowerCase();
  const ac = (el.getAttribute?.('autocomplete') || '').toLowerCase();
  return type === 'password' || ac === 'current-password' || ac === 'new-password' || ac.startsWith('cc-');
};

/** rrweb options derived from the privacy settings. Passwords are always masked; canvas/iframes/scripts are never recorded. */
export function rrwebPrivacyOptions(p: PrivacyOptions) {
  return {
    blockSelector: p.blockSelector,
    // rrweb calls maskTextFn only for text under a matching element; '*' = all text.
    maskTextSelector: p.maskAllText ? '*' : p.maskSelector,
    maskTextFn: (text: string, el: HTMLElement | null): string => {
      try {
        return isUnmasked(el, p) ? text : maskString(text);
      } catch {
        return maskString(text);
      }
    },
    // Every input/textarea/select goes through maskInputFn so the decision is ours (fail closed).
    maskAllInputs: true,
    maskInputFn: (text: string, el: HTMLElement): string => {
      try {
        if (isPassword(el)) return maskString(text);
        if (p.maskAllInputs) return isUnmasked(el, p) ? text : maskString(text);
        return closest(el, p.maskSelector) && !isUnmasked(el, p) ? maskString(text) : text;
      } catch {
        return maskString(text);
      }
    },
    recordCanvas: false,
    recordCrossOriginIframes: false,
    collectFonts: false,
    inlineImages: false,
    slimDOMOptions: { script: true, comment: true, headFavicon: true, headMetaVerification: true },
  };
}
