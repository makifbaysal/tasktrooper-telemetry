import { computeTotal } from "./cart";

export function checkout(items: number[]): number {
  const total = computeTotal(items);
  return total;
}

(globalThis as any).checkout = checkout;
