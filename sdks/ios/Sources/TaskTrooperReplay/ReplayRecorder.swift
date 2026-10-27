#if canImport(UIKit) && os(iOS)
import UIKit
import ObjectiveC

/// Main-thread side of replay. A main run loop observer (after Core Animation's commit) marks the UI dirty
/// whenever the run loop did work; captures run at most every `minInterval` (≥ 0.5 s = 2 fps, raised when a
/// capture is expensive) with a trailing capture so the final state after a burst is recorded. Unchanged
/// frames are dropped by the engine. Touches come from a `UIWindow.sendEvent(_:)` hook (observation only, the
/// original implementation always runs); keyboard visibility from UIKit notifications.
@MainActor
final class ReplayRecorder {
    static private(set) var shared: ReplayRecorder?

    static let minInterval: TimeInterval = 0.5
    static let maxInterval: TimeInterval = 5
    static let moveInterval: TimeInterval = 0.1
    static let sessionCheckInterval: TimeInterval = 5

    private let controller: ReplayController
    private let mapper: UIKitWireframeMapper
    private var observer: CFRunLoopObserver?
    private var lastCapture: CFTimeInterval = 0
    private var lastSessionCheck: CFTimeInterval = 0
    private var trailingScheduled = false
    private var ownWake = false
    private var lastMove: TimeInterval = 0
    private(set) var minInterval = ReplayRecorder.minInterval
    private(set) var lastCost: TimeInterval = 0

    static func install(controller: ReplayController, options: TrooperReplayOptions) {
        guard shared == nil else { return }
        let r = ReplayRecorder(controller: controller, mapper: UIKitWireframeMapper(maskAllText: options.maskAllText))
        shared = r
        r.start()
    }

    init(controller: ReplayController, mapper: UIKitWireframeMapper) {
        self.controller = controller
        self.mapper = mapper
    }

    private func start() {
        // Order > 2_000_000 (CA commit): runs after layout/display of the iteration.
        let obs = CFRunLoopObserverCreateWithHandler(kCFAllocatorDefault, CFRunLoopActivity.beforeWaiting.rawValue, true, 2_100_000) { _, _ in
            MainActor.assumeIsolated { ReplayRecorder.shared?.runLoopWillSleep() }
        }
        if let obs { CFRunLoopAddObserver(CFRunLoopGetMain(), obs, .commonModes) }
        observer = obs
        TouchHook.install()
        let nc = NotificationCenter.default
        nc.addObserver(forName: UIResponder.keyboardWillShowNotification, object: nil, queue: .main) { _ in
            MainActor.assumeIsolated { ReplayRecorder.shared?.keyboard(true) }
        }
        nc.addObserver(forName: UIResponder.keyboardWillHideNotification, object: nil, queue: .main) { _ in
            MainActor.assumeIsolated { ReplayRecorder.shared?.keyboard(false) }
        }
        nc.addObserver(forName: UIApplication.didEnterBackgroundNotification, object: nil, queue: .main) { _ in
            MainActor.assumeIsolated { ReplayRecorder.shared?.controller.flush() }
        }
        runLoopWillSleep()
    }

    // MARK: - Capture scheduling

    private func runLoopWillSleep() {
        if ownWake { ownWake = false; return } // the iteration of our own capture: not a UI change
        let now = CACurrentMediaTime()
        let due = lastCapture + minInterval
        if now >= due {
            capture()
        } else if !trailingScheduled {
            trailingScheduled = true
            DispatchQueue.main.asyncAfter(deadline: .now() + (due - now)) {
                MainActor.assumeIsolated {
                    guard let r = ReplayRecorder.shared else { return }
                    r.trailingScheduled = false
                    r.ownWake = true // this wake-up is ours, not a UI change
                    r.capture()
                }
            }
        }
    }

    private func capture() {
        guard controller.isCapturing.current else {
            // Not recording this session; re-check now and then (a new session gets a new sampling decision).
            let now = CACurrentMediaTime()
            if now - lastSessionCheck >= Self.sessionCheckInterval {
                lastSessionCheck = now
                controller.refreshSession { capturing in
                    guard capturing else { return }
                    DispatchQueue.main.async {
                        MainActor.assumeIsolated {
                            ReplayRecorder.shared?.ownWake = true
                            ReplayRecorder.shared?.capture()
                        }
                    }
                }
            }
            return
        }
        let windows = Self.windows()
        guard let base = windows.first(where: { $0.isKeyWindow }) ?? windows.first else { return }
        let ts = Int64((Date().timeIntervalSince1970 * 1000).rounded())
        let result = mapper.map(windows: windows)
        lastCapture = CACurrentMediaTime()
        lastCost = result.cost
        // Budget ≈ 1% of main-thread time: interval = cost × 100, clamped to [0.5 s, 5 s].
        minInterval = min(max(result.cost * 100, Self.minInterval), Self.maxInterval)
        if result.truncated { minInterval = max(minInterval, 2) }
        let size = base.bounds.size
        controller.frame(ts: ts, width: Int(size.width.rounded()), height: Int(size.height.rounded()),
                         density: Double(base.screen.scale), nodes: result.nodes)
    }

    /// App windows back to front, excluding keyboard / text-effects windows (they can show typed text).
    static func windows() -> [UIWindow] {
        guard let app = UIApplication.value(forKey: "sharedApplication") as? UIApplication else { return [] }
        var all: [UIWindow] = []
        for scene in app.connectedScenes {
            guard let ws = scene as? UIWindowScene, ws.activationState != .background, ws.activationState != .unattached else { continue }
            all.append(contentsOf: ws.windows)
        }
        return all.enumerated()
            .filter { !$0.element.isHidden && $0.element.alpha > 0.01 && !isSystemWindow($0.element) }
            .sorted { ($0.element.windowLevel.rawValue, $0.offset) < ($1.element.windowLevel.rawValue, $1.offset) }
            .map(\.element)
    }

    private static func isSystemWindow(_ w: UIWindow) -> Bool {
        let name = NSStringFromClass(type(of: w))
        return name.contains("Keyboard") || name.contains("TextEffects") || name.contains("InputWindow")
    }

    // MARK: - Touches & keyboard

    fileprivate func touches(_ event: UIEvent, window: UIWindow) {
        guard controller.isCapturing.current, let touches = event.allTouches else { return }
        let ts = Int64((Date().timeIntervalSince1970 * 1000).rounded())
        for t in touches {
            let phase: String
            switch t.phase {
            case .began: phase = "down"
            case .moved:
                guard t.timestamp - lastMove >= Self.moveInterval else { continue }
                lastMove = t.timestamp
                phase = "move"
            case .ended, .cancelled: phase = "up"
            default: continue
            }
            let p = t.location(in: nil)
            controller.touch(ts: ts, x: Double(p.x + window.frame.minX), y: Double(p.y + window.frame.minY), phase: phase)
        }
    }

    private func keyboard(_ visible: Bool) {
        controller.keyboard(ts: Int64((Date().timeIntervalSince1970 * 1000).rounded()), visible: visible)
    }
}

/// Observes touch events by wrapping `UIWindow.sendEvent(_:)` once. The original implementation always runs;
/// observation is skipped (and errors cannot propagate) when replay is not capturing.
@MainActor
enum TouchHook {
    private static var installed = false

    static func install() {
        guard !installed else { return }
        installed = true
        let sel = #selector(UIWindow.sendEvent(_:))
        guard let method = class_getInstanceMethod(UIWindow.self, sel) else { return }
        typealias Fn = @convention(c) (UIWindow, Selector, UIEvent) -> Void
        let original = unsafeBitCast(method_getImplementation(method), to: Fn.self)
        let block: @convention(block) (UIWindow, UIEvent) -> Void = { window, event in
            if event.type == .touches {
                MainActor.assumeIsolated { ReplayRecorder.shared?.touches(event, window: window) }
            }
            original(window, sel, event)
        }
        method_setImplementation(method, imp_implementationWithBlock(block))
    }
}
#endif

#if canImport(SwiftUI) && canImport(UIKit) && os(iOS)
import SwiftUI

@available(iOS 13.0, *)
struct TrooperUnmaskMarker: UIViewRepresentable {
    let unmask: Bool
    func makeUIView(context: Context) -> TrooperUnmaskMarkerView { TrooperUnmaskMarkerView(frame: .zero) }
    func updateUIView(_ view: TrooperUnmaskMarkerView, context: Context) { view.unmask = unmask }
}

@available(iOS 13.0, *)
public extension View {
    /// Session replay: record text inside this view unmasked (`true`) or force masking (`false`).
    /// Text fields and secure fields stay masked.
    func trooperUnmask(_ unmask: Bool = true) -> some View {
        background(TrooperUnmaskMarker(unmask: unmask))
    }
}
#endif
