package com.example.shop;

public class PriceValidator {
    public int validate(int price) {
        if (price < 0) {
            fail(price);
        }
        return price;
    }

    private void fail(int price) {
        throw new IllegalStateException("negative price: " + price);
    }
}
