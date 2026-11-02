import Foundation
import FirebaseFirestore

enum API {
    static let base = URL(string: "https://api.shop.example.com/v1")!
    // static let old = URL(string: "https://commented.example.com")!
    static let interpolated = "https://\(host)/x"
}

func fetchOrders() async throws -> Data {
    let (data, _) = try await URLSession.shared.data(from: API.base.appendingPathComponent("orders"))
    _ = Firestore.firestore()
    return data
}
