import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { actionName, selectorOf } from '../src/actions';
import { setup, teardown } from './helpers';

beforeEach(() => {
  document.body.innerHTML = `
    <div id="app">
      <button class="btn primary css-1x2y3z" aria-label="Add to cart">+</button>
      <button class="buy">  Buy   now <span>!</span></button>
      <a href="#" class="link">Docs</a>
      <input type="text" value="secret@example.com" />
    </div>`;
});
afterEach(async () => {
  vi.useRealTimers();
  await teardown();
});

const click = (el: Element) => el.dispatchEvent(new MouseEvent('click', { bubbles: true }));
const actions = async (events: () => Promise<{ type: string; action?: unknown }[]>) =>
  (await events()).filter((e) => e.type === 'ACTION').map((e) => e.action);

describe('click actions', () => {
  it('derives names and selectors without capturing input values', () => {
    const [b1, b2] = document.querySelectorAll('button');
    expect(actionName(b1!)).toBe('Add to cart');
    expect(actionName(b2!)).toBe('Buy now !');
    expect(actionName(document.querySelector('input')!)).toBe('input');
    expect(selectorOf(b1!)).toBe('div#app > button.btn.primary');
  });

  it('rage click: ≥3 clicks on the same element within 1 s → one rage action', async () => {
    vi.useFakeTimers();
    const { events } = setup();
    const b = document.querySelector('.buy')!;
    click(b);
    vi.advanceTimersByTime(200);
    click(b.querySelector('span')!); // inner element resolves to the same button
    vi.advanceTimersByTime(200);
    click(b);
    vi.advanceTimersByTime(1000);
    vi.useRealTimers();
    expect(await actions(events)).toEqual([expect.objectContaining({ name: 'Buy now !', rage: true })]);
  });

  it('two clicks are not rage', async () => {
    vi.useFakeTimers();
    const { events } = setup();
    const b = document.querySelector('.buy')!;
    click(b);
    click(b);
    vi.advanceTimersByTime(1000);
    vi.useRealTimers();
    expect(await actions(events)).toEqual([expect.objectContaining({ rage: false })]);
  });

  it('dead click: no DOM mutation / navigation / request within 1 s', async () => {
    vi.useFakeTimers();
    const { events } = setup();
    click(document.querySelector('.buy')!);
    await vi.advanceTimersByTimeAsync(1000);
    vi.useRealTimers();
    expect(await actions(events)).toEqual([expect.objectContaining({ dead: true, rage: false })]);
  });

  it('click followed by a DOM change is not dead', async () => {
    vi.useFakeTimers();
    const { events } = setup();
    const b = document.querySelector('.buy')!;
    b.addEventListener('click', () => document.body.append(document.createElement('dialog')));
    click(b);
    await vi.advanceTimersByTimeAsync(1000);
    vi.useRealTimers();
    expect(await actions(events)).toEqual([expect.objectContaining({ dead: false })]);
  });

  it('click followed by a request or navigation is not dead; inputs are never dead', async () => {
    vi.useFakeTimers();
    const { events } = setup();
    const [b1] = document.querySelectorAll('button');
    b1!.addEventListener('click', () => void fetch('/api/cart'));
    click(b1!);
    const link = document.querySelector('.link')!;
    link.addEventListener('click', (e) => {
      e.preventDefault();
      history.pushState(null, '', '/docs');
    });
    click(link);
    click(document.querySelector('input')!);
    await vi.advanceTimersByTimeAsync(1000);
    vi.useRealTimers();
    expect((await actions(events)).map((a) => (a as { dead: boolean }).dead)).toEqual([false, false, false]);
  });
});
