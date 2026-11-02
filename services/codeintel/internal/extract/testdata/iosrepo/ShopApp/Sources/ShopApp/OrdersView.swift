import SwiftUI

struct Order: Identifiable, Hashable { let id: String }

struct OrdersView: View {
    let orders: [Order] = []
    @State private var showFilters = false

    var body: some View {
        NavigationStack {
            List(orders) { order in
                NavigationLink(value: order) { OrderRow(order: order) }
            }
            .navigationDestination(for: Order.self) { order in
                OrderDetailView(order: order)
            }
            .sheet(isPresented: $showFilters) { FiltersSheet() }
        }
    }
}

struct OrderRow: View {
    let order: Order
    var body: some View { Text(order.id) }
}

struct OrderDetailView: View {
    let order: Order
    var body: some View { Text("Order \(order.id)") }
}

struct FiltersSheet: View {
    var body: some View { Text("Filters") }
}

struct ProfileView: View {
    var body: some View { Text("Profile") }
}

struct PromoBanner: View {
    var body: some View { Text("Sale \("now")") }
}

struct SettingsScreen: View {
    var body: some View { Text("Settings") }
}
