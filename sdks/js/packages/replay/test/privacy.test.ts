import { afterEach, describe, expect, it, vi } from 'vitest';
import { isUnmasked, maskString, rrwebPrivacyOptions, type PrivacyOptions } from '../src/privacy';
import { rr, setup, teardown } from './helpers';

vi.mock('@rrweb/record', async () => (await import('./rrweb-mock')).rrwebMock());

afterEach(teardown);

const defaults: PrivacyOptions = {
  maskAllText: true,
  maskAllInputs: true,
  blockSelector: '[data-trooper-block]',
  maskSelector: '[data-trooper-mask]',
  unmaskSelector: '[data-trooper-unmask]',
};

const dom = (html: string) => {
  document.body.innerHTML = html;
  return (sel: string) => document.querySelector(sel) as HTMLElement;
};

describe('options passed to rrweb', () => {
  it('privacy defaults (contracts §23)', () => {
    vi.spyOn(Math, 'random').mockReturnValue(0);
    setup({ sampleRate: 1 });
    const o = rr.opts!;
    expect(o).toMatchObject({
      blockSelector: '[data-trooper-block]',
      maskTextSelector: '*',
      maskAllInputs: true,
      recordCanvas: false,
      recordCrossOriginIframes: false,
      collectFonts: false,
      inlineImages: false,
      slimDOMOptions: { script: true, comment: true },
    });
    expect(typeof o.maskTextFn).toBe('function');
    expect(typeof o.maskInputFn).toBe('function');
    expect(typeof o.errorHandler).toBe('function');
    expect(o.errorHandler(new Error('observer failed'))).toBe(true);
    expect(o.checkoutEveryNms).toBeUndefined(); // checkouts are driven by the SDK (60 s buffer / 5 min session)
  });

  it('custom selectors and maskAllText: false', () => {
    vi.spyOn(Math, 'random').mockReturnValue(0);
    setup({ sampleRate: 1, maskAllText: false, blockSelector: '.secret', maskSelector: '.pii', unmaskSelector: '.public' });
    expect(rr.opts).toMatchObject({ blockSelector: '.secret', maskTextSelector: '.pii' });
  });
});

describe('text masking', () => {
  it('replaces characters with * and keeps length/whitespace', () => {
    expect(maskString('Jane Doe\n42')).toBe('**** ***\n**');
  });

  it('masks all text unless under data-trooper-unmask; nearest of mask/unmask wins', () => {
    const $ = dom(`
      <p id="a">secret</p>
      <div data-trooper-unmask><p id="b">Total</p><span data-trooper-mask><b id="c">card</b></span></div>
      <div data-trooper-mask><p data-trooper-unmask id="d">ok</p></div>`);
    const fn = rrwebPrivacyOptions(defaults).maskTextFn;
    expect(fn('secret', $('#a'))).toBe('******');
    expect(fn('Total', $('#b'))).toBe('Total');
    expect(fn('card', $('#c'))).toBe('****');
    expect(fn('ok', $('#d'))).toBe('ok');
    expect(fn('orphan', null)).toBe('******');
    expect(isUnmasked($('#b'), defaults)).toBe(true);
  });

  it('invalid custom selectors fail closed', () => {
    const $ = dom('<p id="a">x</p>');
    const fn = rrwebPrivacyOptions({ ...defaults, unmaskSelector: '[[[' }).maskTextFn;
    expect(fn('hello', $('#a'))).toBe('*****');
  });
});

describe('input masking', () => {
  it('maskAllInputs: masked unless unmasked; passwords always masked', () => {
    const $ = dom(`
      <input id="email" type="email">
      <div data-trooper-unmask><input id="search" type="search"><input id="pw" type="password"><input id="cc" autocomplete="cc-number"></div>`);
    const fn = rrwebPrivacyOptions(defaults).maskInputFn;
    expect(fn('a@b.co', $('#email'))).toBe('******');
    expect(fn('shoes', $('#search'))).toBe('shoes');
    expect(fn('hunter2', $('#pw'))).toBe('*******');
    expect(fn('4111', $('#cc'))).toBe('****');
  });

  it('maskAllInputs: false still masks passwords and maskSelector', () => {
    const $ = dom(`<input id="q"><input id="pw" type="password"><input id="ssn" data-trooper-mask>`);
    const fn = rrwebPrivacyOptions({ ...defaults, maskAllInputs: false }).maskInputFn;
    expect(fn('query', $('#q'))).toBe('query');
    expect(fn('hunter2', $('#pw'))).toBe('*******');
    expect(fn('123', $('#ssn'))).toBe('***');
  });
});
