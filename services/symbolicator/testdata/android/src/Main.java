package com.example.shop;

public class Main {
    public static void main(String[] args) {
        CartService svc = new CartService();
        try {
            svc.checkout(new int[] {1, 2, -3});
        } catch (RuntimeException e) {
            e.printStackTrace();
        }
    }
}
