package com.tasktrooper.sample

import android.os.Bundle
import androidx.activity.ComponentActivity
import androidx.activity.compose.setContent
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.padding
import androidx.compose.material3.Button
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Text
import androidx.compose.ui.Modifier
import androidx.compose.ui.unit.dp
import androidx.navigation.compose.NavHost
import androidx.navigation.compose.composable
import androidx.navigation.compose.rememberNavController
import com.tasktrooper.android.TaskTrooper
import com.tasktrooper.android.compose.TrooperNavigationTracking
import com.tasktrooper.android.compose.trooperAction
import com.tasktrooper.android.okhttp.TrooperInterceptor
import okhttp3.OkHttpClient
import okhttp3.Request
import kotlin.concurrent.thread

class MainActivity : ComponentActivity() {
    private val http = OkHttpClient.Builder().addInterceptor(TrooperInterceptor()).build()

    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        setContent {
            MaterialTheme {
                val nav = rememberNavController()
                TrooperNavigationTracking(nav)
                NavHost(nav, startDestination = "home") {
                    composable("home") {
                        Column(Modifier.fillMaxSize().padding(24.dp), verticalArrangement = Arrangement.spacedBy(12.dp)) {
                            Text("TaskTrooper sample")
                            Button(onClick = { nav.navigate("product/42") }, modifier = Modifier.trooperAction("open_product")) {
                                Text("Open product")
                            }
                            Button(onClick = { fetch() }, modifier = Modifier.trooperAction("fetch")) { Text("HTTP request") }
                            Button(onClick = { TaskTrooper.track("checkout_started", mapOf("plan" to "pro"), 120.0) }) {
                                Text("Track custom event")
                            }
                            Button(onClick = { TaskTrooper.captureError(IllegalStateException("handled")) }) {
                                Text("Capture handled error")
                            }
                            Button(onClick = { Thread.sleep(6_000) }) { Text("Freeze main thread (ANR)") }
                            Button(onClick = { throw RuntimeException("sample crash") }) { Text("Crash") }
                        }
                    }
                    composable("product/{id}") { entry ->
                        Text("Product ${entry.arguments?.getString("id")}", Modifier.padding(24.dp))
                    }
                }
            }
        }
    }

    private fun fetch() = thread {
        try {
            http.newCall(Request.Builder().url("https://api.example.com/v1/ping?token=secret").build()).execute().close()
        } catch (_: Exception) {
        }
    }
}
