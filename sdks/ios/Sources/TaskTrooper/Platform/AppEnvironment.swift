import Foundation
#if canImport(UIKit)
import UIKit
#endif

/// Host process kind. The SDK is built with extension-safe APIs only (`APPLICATION_EXTENSION_API_ONLY`),
/// so it can be linked into app extensions (widgets, share, notification service, …).
enum AppEnvironment {
    /// True inside an app extension (`.appex` bundle), where `UIApplication.shared` does not exist.
    static let isAppExtension: Bool = isExtension(bundlePath: Bundle.main.bundlePath)

    static func isExtension(bundlePath: String) -> Bool {
        var path = bundlePath
        while path.hasSuffix("/") { path.removeLast() }
        return path.hasSuffix(".appex")
    }

    #if canImport(UIKit)
    /// `UIApplication.shared` in an app, nil in an extension. Looked up dynamically because the
    /// property is unavailable when compiling with APPLICATION_EXTENSION_API_ONLY.
    @MainActor
    static var sharedApplication: UIApplication? {
        guard !isAppExtension else { return nil }
        return UIApplication.value(forKey: "sharedApplication") as? UIApplication
    }
    #endif
}
