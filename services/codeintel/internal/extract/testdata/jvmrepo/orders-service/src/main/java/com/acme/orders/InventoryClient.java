package com.acme.orders;

import org.springframework.cloud.openfeign.FeignClient;
import org.springframework.web.bind.annotation.GetMapping;

@FeignClient(name = "inventory", url = "https://inventory.example.com")
public interface InventoryClient {
    @GetMapping("/stock/{sku}")
    Stock stock(@PathVariable("sku") String sku);
}
