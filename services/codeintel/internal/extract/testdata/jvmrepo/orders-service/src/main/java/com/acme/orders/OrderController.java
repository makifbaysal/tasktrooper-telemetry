package com.acme.orders;

import java.util.List;
import org.postgresql.Driver;
import org.springframework.data.jpa.repository.JpaRepository;
import org.springframework.web.bind.annotation.*;
import org.springframework.web.client.RestTemplate;

@RestController
@RequestMapping("/api/v1/orders")
public class OrderController {
    static final String ITEMS = "/{orderId}/items";
    private final RestTemplate restTemplate = new RestTemplate();

    @GetMapping
    public List<Order> list() {
        return repo.findAll();
    }

    @GetMapping("/{id}")
    public Order get(@PathVariable("id") String id) {
        return repo.findById(id);
    }

    @PostMapping(value = {"", "/bulk"}, consumes = "application/json")
    public Order create(@RequestBody Order o) {
        restTemplate.postForObject("https://api.stripe.com/v1/charges", o, String.class);
        return o;
    }

    @RequestMapping(value = ITEMS, method = {RequestMethod.PUT, RequestMethod.PATCH})
    public void items() {
    }

    @RequestMapping("/legacy")
    public void legacy() {
    }

    // @GetMapping("/commented")
    /* @DeleteMapping("/block-commented") */
    public String doc() {
        return "@GetMapping(\"/in-string\") and a } brace";
    }

    @DeleteMapping(path = "/{id}")
    public void delete(@PathVariable String id) {
    }
}
