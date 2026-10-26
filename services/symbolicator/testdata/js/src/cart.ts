export function computeTotal(items: number[]): number {
  let sum = 0;
  for (const it of items) {
    sum += validatePrice(it);
  }
  return sum;
}

function validatePrice(price: number): number {
  if (price < 0) {
    throw new Error("negative price: " + price);
  }
  return price;
}
