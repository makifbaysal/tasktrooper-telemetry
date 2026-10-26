import Foundation

/// Adopt on a view controller to control its tracked name. Return nil to not track it.
public protocol TrooperViewNaming {
    var trooperViewName: String? { get }
}

/// Platform-independent rules for automatic (UIKit) view tracking.
enum ViewNameFilter {
    static let containerClassNames: Set<String> = [
        "UINavigationController", "UITabBarController", "UIPageViewController",
        "UISplitViewController", "UIInputViewController",
    ]

    /// - Parameters:
    ///   - className: runtime class name (`NSStringFromClass`)
    ///   - ancestors: superclass names (nearest first)
    ///   - isContainer: the controller is a known container (navigation/tab/page/split)
    ///   - isAppCode: the class lives in the app bundle or an embedded framework (not a system framework)
    static func shouldTrack(className: String, ancestors: [String], isContainer: Bool, isAppCode: Bool, ignored: Set<String> = []) -> Bool {
        if isContainer { return false }
        if containerClassNames.contains(className) { return false }
        if ancestors.contains(where: { containerClassNames.contains($0) }) { return false }
        if className.hasPrefix("_UI") || className.hasPrefix("UI") && !isAppCode { return false }
        if !isAppCode { return false }
        let all = [className] + ancestors
        if all.contains(where: { $0.contains("UIHostingController") || $0.contains("HostingController<") }) { return false }
        let display = displayName(className)
        if ignored.contains(className) || ignored.contains(display) { return false }
        return true
    }

    /// "MyApp.CheckoutViewController" → "CheckoutViewController"; mangled `_TtC…` names are demangled
    /// by the caller via `String(describing:)`, so only the module prefix is stripped here.
    static func displayName(_ className: String) -> String {
        guard !className.contains("<"), let dot = className.lastIndex(of: ".") else { return className }
        return String(className[className.index(after: dot)...])
    }
}
