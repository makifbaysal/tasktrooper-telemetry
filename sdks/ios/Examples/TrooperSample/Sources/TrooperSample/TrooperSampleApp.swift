import SwiftUI
import TaskTrooper
import TaskTrooperSwiftUI
import TaskTrooperReplay

@main
struct TrooperSampleApp: App {
    init() {
        var config = TrooperConfig(
            dsn: "http://tt_dev_ingest_key@localhost:4318/1",
            app: "sample-ios",
            version: "1.0.0",
            environment: "development",
            tracePropagationTargets: ["localhost", "api.example.com"]
        )
        config.debug = true
        // Wireframe session replay (no screenshots); every session in the sample, text masked by default.
        TaskTrooper.start(config, replay: .init(sampleRate: 1.0, onErrorSampleRate: 1.0))
    }

    var body: some Scene {
        WindowGroup {
            HomeScreen()
        }
    }
}

struct HomeScreen: View {
    @State private var showCheckout = false
    @State private var status = ""
    @State private var password = ""

    var body: some View {
        VStack(spacing: 16) {
            Text("TaskTrooper sample").font(.headline)
                .trooperUnmask() // static copy: safe to show in replay
            SecureField("Password", text: $password) // always masked in replay
            Button("Log in as demo user") {
                TaskTrooper.identify(id: "demo-user", traits: ["plan": "pro"])
            }
            Button("Load products") {
                TaskTrooper.trackAction("load_products")
                Task {
                    let url = URL(string: "https://api.example.com/products?page=1")!
                    let result = try? await URLSession.shared.data(from: url)
                    status = result == nil ? "request failed" : "loaded"
                }
            }
            Button("Report handled error") {
                TaskTrooper.captureError(URLError(.badServerResponse), attributes: ["screen": "home"])
            }
            Button("Checkout") { showCheckout = true }
            Text(status).font(.footnote)
        }
        .padding()
        .trooperView("Home")
        .sheet(isPresented: $showCheckout) {
            CheckoutScreen()
        }
    }
}

struct CheckoutScreen: View {
    var body: some View {
        VStack(spacing: 16) {
            Text("Checkout")
            Button("Pay") {
                TaskTrooper.track("checkout_completed", properties: ["items": "3"], value: 120)
            }
        }
        .trooperView("Checkout")
    }
}
