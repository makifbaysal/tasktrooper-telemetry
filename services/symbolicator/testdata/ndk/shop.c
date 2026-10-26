extern void abort(void);

static int validate_price(int price) {
    if (price < 0) {
        abort();
    }
    return price;
}

__attribute__((noinline)) int compute_total(const int *items, int n) {
    int sum = 0;
    for (int i = 0; i < n; i++) {
        sum += validate_price(items[i]);
    }
    return sum;
}

int Java_com_example_shop_Native_total(void *env, void *thiz, const int *items, int n) {
    return compute_total(items, n);
}
