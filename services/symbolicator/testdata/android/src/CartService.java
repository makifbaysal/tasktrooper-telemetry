package com.example.shop;

public class CartService {
    private final PriceValidator validator = new PriceValidator();

    public int checkout(int[] items) {
        int total = computeTotal(items);
        if (total > 1000) {
            System.out.println("big order");
        }
        return total;
    }

    private int computeTotal(int[] items) {
        int sum = 0;
        for (int it : items) {
            sum += validator.validate(it);
        }
        return sum;
    }
}
