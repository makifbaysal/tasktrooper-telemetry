#if canImport(SwiftUI)
import SwiftUI
#if SWIFT_PACKAGE
import TaskTrooper
#endif

@available(iOS 13.0, tvOS 13.0, macOS 11.0, *)
struct TrooperViewModifier: ViewModifier {
    let name: String
    let attributes: [String: String]
    @State private var key = "swiftui-" + UUID().uuidString

    func body(content: Content) -> some View {
        content
            .onAppear { TaskTrooper.startView(name, key: key, attributes: attributes) }
            .onDisappear { TaskTrooper.stopView(key) }
    }
}

@available(iOS 13.0, tvOS 13.0, macOS 11.0, *)
public extension View {
    /// Tracks this SwiftUI view as a RUM view (screen) named `name`.
    ///
    /// ```swift
    /// CheckoutScreen().trooperView("Checkout")
    /// ```
    func trooperView(_ name: String, attributes: [String: String] = [:]) -> some View {
        modifier(TrooperViewModifier(name: name, attributes: attributes))
    }
}
#endif
