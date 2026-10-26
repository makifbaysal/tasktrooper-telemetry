import Foundation
#if canImport(UIKit)
import UIKit
#endif

enum DeviceInfo {
    static let sdkName = "TaskTrooper-iOS"
    static let sdkVersion = "0.1.0"

    /// Hardware model identifier, e.g. "iPhone16,1" (simulator: the simulated model).
    static func modelIdentifier() -> String {
        if let sim = ProcessInfo.processInfo.environment["SIMULATOR_MODEL_IDENTIFIER"] { return sim }
        var u = utsname()
        uname(&u)
        return withUnsafeBytes(of: &u.machine) { raw in
            String(decoding: raw.prefix { $0 != 0 }, as: UTF8.self)
        }
    }

    static func osName() -> String {
        #if os(iOS)
        return ProcessInfo.processInfo.isMacCatalystApp ? "macOS" : "iOS"
        #elseif os(tvOS)
        return "tvOS"
        #elseif os(macOS)
        return "macOS"
        #else
        return "unknown"
        #endif
    }

    static func osVersion() -> String {
        let v = ProcessInfo.processInfo.operatingSystemVersion
        return "\(v.majorVersion).\(v.minorVersion).\(v.patchVersion)"
    }

    /// Info that does not need the main thread.
    static func baseDevice() -> RumDevice {
        var type = "mobile"
        #if os(tvOS)
        type = "tv"
        #elseif os(macOS)
        type = "desktop"
        #endif
        return RumDevice(type: type, manufacturer: "Apple", model: modelIdentifier(),
                         locale: Locale.current.identifier, screenWidth: nil, screenHeight: nil)
    }

    #if canImport(UIKit)
    /// Idiom and screen size (main thread only).
    @MainActor
    static func refine(_ device: inout RumDevice) {
        #if os(iOS)
        var onMac = ProcessInfo.processInfo.isMacCatalystApp
        if #available(iOS 14.0, *), ProcessInfo.processInfo.isiOSAppOnMac { onMac = true }
        if onMac {
            device.type = "desktop"
        } else if UIDevice.current.userInterfaceIdiom == .pad {
            device.type = "tablet"
        }
        #endif
        let size = UIScreen.main.nativeBounds.size
        device.screenWidth = Int32(size.width)
        device.screenHeight = Int32(size.height)
    }
    #endif
}
