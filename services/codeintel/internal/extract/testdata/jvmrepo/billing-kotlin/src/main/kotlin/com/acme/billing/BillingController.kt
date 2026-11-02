package com.acme.billing

import java.util.concurrent.TimeUnit
import org.springframework.kafka.annotation.KafkaListener
import org.springframework.scheduling.annotation.Scheduled
import org.springframework.web.bind.annotation.*
import org.springframework.web.reactive.function.client.WebClient
import org.springframework.web.reactive.function.server.coRouter

const val TOPIC = "ledger.entries"
const val PAYPAL = "https://api.paypal.com"

@RestController
@RequestMapping(BillingController.BASE)
class BillingController(private val svc: BillingService) {
    companion object {
        const val BASE = "/billing"
    }

    private val client = WebClient.builder().baseUrl(PAYPAL).build()

    @GetMapping(value = ["/invoices", "/bills"])
    suspend fun invoices(): List<Invoice> = svc.all()

    @PostMapping("/invoices/{id}/pay")
    fun pay(@PathVariable id: String) {
    }

    @KafkaListener(id = "billing-ledger", topics = [TOPIC])
    fun onLedger(rec: String) {
    }

    @Scheduled(fixedDelay = 30, timeUnit = TimeUnit.SECONDS) fun sweep() {}

    /*
    @GetMapping("/block-commented")
    fun old() {}
    */
}

@Configuration
class Routes {
    @Bean
    fun fnRoutes(h: StatusHandler) = coRouter {
        "/kfn".nest {
            GET("/status", h::status)
        }
        POST("/kfn-top", h::top)
    }
}
