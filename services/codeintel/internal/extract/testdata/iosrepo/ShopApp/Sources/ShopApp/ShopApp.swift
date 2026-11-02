import SwiftUI
import TaskTrooper

@main
struct ShopApp: App {
    init() {
        TaskTrooper.start(TrooperConfig(dsn: "https://key@telemetry.example.com/1", app: "shop-ios", version: "1.0.0"))
    }

    var body: some Scene {
        WindowGroup {
            RootTabView()
        }
    }
}
