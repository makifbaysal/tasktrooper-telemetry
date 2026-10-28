// No imports from src: loaded from the vi.mock factory of @rrweb/record.

/** Shared state of the `@rrweb/record` mock (see `vi.mock` in each test file). */
export interface RrState {
  opts: Record<string, any> | undefined;
  calls: number;
  stops: number;
  snapshots: number;
  throwOnRecord: boolean;
}

export const rr: RrState = { opts: undefined, calls: 0, stops: 0, snapshots: 0, throwOnRecord: false };

export const META = (ts = Date.now()) => ({ type: 4, data: { href: 'http://localhost/', width: 1024, height: 768 }, timestamp: ts });
export const FULL = (ts = Date.now()) => ({ type: 2, data: { node: { type: 0, childNodes: [], id: 1 }, initialOffset: { top: 0, left: 0 } }, timestamp: ts });
export const CLICK = (id = 5, ts = Date.now()) => ({ type: 3, data: { source: 2, type: 2, id, x: 1, y: 1 }, timestamp: ts });
export const INPUT = (id = 7, ts = Date.now()) => ({ type: 3, data: { source: 5, text: '***', isChecked: false, id }, timestamp: ts });
export const MUTATION = (bytes = 10, ts = Date.now()) => ({
  type: 3,
  data: { source: 0, texts: [], attributes: [], removes: [], adds: [{ parentId: 1, nextId: null, node: { type: 3, textContent: 'x'.repeat(bytes), id: 9 } }] },
  timestamp: ts,
});

/** Mock factory for `vi.mock('@rrweb/record', ...)`. */
export function rrwebMock() {
  const record = Object.assign(
    (o: Record<string, any>) => {
      rr.calls++;
      if (rr.throwOnRecord) throw new Error('rrweb boom');
      rr.opts = o;
      o.emit(META());
      o.emit(FULL());
      return () => {
        rr.stops++;
      };
    },
    {
      takeFullSnapshot: (isCheckout?: boolean) => {
        rr.snapshots++;
        rr.opts?.emit(META(), isCheckout);
        rr.opts?.emit(FULL());
      },
    },
  );
  return { record };
}

/** Emit an rrweb event through the recorder's `emit` callback. */
export const emit = (e: unknown, isCheckout?: boolean) => rr.opts!.emit(e, isCheckout);
