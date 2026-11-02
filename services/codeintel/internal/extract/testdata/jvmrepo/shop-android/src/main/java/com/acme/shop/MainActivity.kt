package com.acme.shop

import androidx.appcompat.app.AppCompatActivity
import androidx.fragment.app.Fragment
import androidx.navigation.compose.NavHost
import androidx.navigation.compose.composable
import retrofit2.Retrofit
import retrofit2.http.GET
import retrofit2.http.Path

object Routes {
    const val CART = "cart"
}

class MainActivity : AppCompatActivity() {
    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        setContent {
            NavHost(nav, startDestination = "home") {
                composable("home") { Home() }
                composable("product/{productId}?ref={ref}") { Product() }
                composable(route = Routes.CART) { Cart() }
                composable<CheckoutRoute> { Checkout() }
                // composable("commented") { }
            }
        }
    }
}

abstract class BaseActivity : AppCompatActivity()

class SettingsActivity : BaseActivity() {
}

class ProfileFragment : Fragment(R.layout.profile)

interface ShopApi {
    @GET("users/{id}")
    suspend fun user(@Path("id") id: String): User
}

val retrofit = Retrofit.Builder()
    .baseUrl("https://api.shop.example.com/")
    .build()
