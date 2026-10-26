#include <stdio.h>
#include <stdlib.h>

static int validate_price(int price) {
    if (price < 0) {
        abort();
    }
    return price;
}

int compute_total(const int *items, int n) {
    int sum = 0;
    for (int i = 0; i < n; i++) {
        sum += validate_price(items[i]);
    }
    return sum;
}
