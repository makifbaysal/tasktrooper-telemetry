import { afterEach, beforeAll, describe, expect, it } from 'vitest';
import { record } from '@rrweb/record';
import { rrwebPrivacyOptions } from '../src/privacy';

// Real @rrweb/record 2.2.0 (no mock): the FullSnapshot must already be masked.
let stop: (() => void) | undefined;

// happy-dom exposes these names on window without values; rrweb's polyfill reads their prototypes.
beforeAll(() => {
  const w = window as any;
  for (const [name, sample] of [
    ['NodeList', () => document.childNodes],
    ['DOMTokenList', () => document.body.classList],
  ] as const) {
    if (!w[name]) Object.defineProperty(w, name, { value: sample().constructor, configurable: true, writable: true });
  }
});
afterEach(() => stop?.());

describe('@rrweb/record with our privacy options', () => {
  it('FullSnapshot masks inputs and passwords (even when unmasked); blocks and drops scripts', () => {
    document.body.innerHTML = `
      <h1>Jane Doe</h1>
      <p data-trooper-unmask>Order total</p>
      <input id="e" type="email" value="jane@example.com">
      <div data-trooper-unmask><input id="pw" type="password" value="hunter2"></div>
      <div data-trooper-block><span>Hidden card</span></div>
      <script>var secret = 1;</script>`;
    const events: any[] = [];
    stop =
      record({
        emit: (e: any) => events.push(e),
        ...rrwebPrivacyOptions({
          maskAllText: true,
          maskAllInputs: true,
          blockSelector: '[data-trooper-block]',
          maskSelector: '[data-trooper-mask]',
          unmaskSelector: '[data-trooper-unmask]',
        }),
      } as any) ?? undefined;
    const full = events.find((e) => e.type === 2);
    expect(full).toBeDefined();
    const json = JSON.stringify(full);
    expect(json).not.toContain('Jane Doe');
    // (Text-node content is empty under happy-dom's untainted accessors; maskTextFn is unit-tested in privacy.test.ts.)
    expect(json).not.toContain('jane@example.com');
    expect(json).toContain('"value":"****************"');
    expect(json).not.toContain('hunter2');
    expect(json).not.toContain('Hidden card');
    expect(json).toContain('rr_width'); // blocked element kept as a sized placeholder
    expect(json).not.toContain('var secret');
  });
});
