package com.acme.orders;

@RestController
public class FakeController {
    @GetMapping("/from-test")
    public String x() { return ""; }
}
