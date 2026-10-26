#if canImport(UIKit)
import UIKit
import ObjectiveC
#if canImport(TaskTrooperCrashC)
import TaskTrooperCrashC
#endif

/// Main-thread instrumentation: views, app start, frames, lifecycle, MetricKit.
@MainActor
enum UIKitIntegration {
    private static var installed = false
    private static var lifecycle: LifecycleObserver?

    static func install(core: TrooperCore, config: TrooperConfig) {
        guard !installed else { return }
        installed = true

        var device = DeviceInfo.baseDevice()
        DeviceInfo.refine(&device)
        core.updateDevice(device)

        if config.trackViews { ViewControllerSwizzler.install(ignored: config.ignoredViewControllers) }
        let observer = LifecycleObserver(core: core, config: config)
        observer.start()
        lifecycle = observer

        #if os(iOS)
        if config.metricKit, #available(iOS 14.0, *) {
            MetricKitReporter.install(core: core)
        }
        #endif
    }
}

// MARK: - UIViewController swizzling

@MainActor
enum ViewControllerSwizzler {
    private static var ignored: Set<String> = []
    private static var loadTimeKey: UInt8 = 0
    private static var nameCache: [ObjectIdentifier: String?] = [:]

    static func install(ignored: Set<String>) {
        self.ignored = ignored
        swizzle(#selector(UIViewController.viewDidLoad)) { vc in
            objc_setAssociatedObject(vc, &loadTimeKey, NSNumber(value: ProcessInfo.processInfo.systemUptime), .OBJC_ASSOCIATION_RETAIN_NONATOMIC)
        }
        swizzleAnimated(#selector(UIViewController.viewDidAppear(_:))) { vc in didAppear(vc) }
        swizzleAnimated(#selector(UIViewController.viewDidDisappear(_:))) { vc in didDisappear(vc) }
    }

    private static func swizzle(_ sel: Selector, after: @escaping @MainActor (UIViewController) -> Void) {
        guard let method = class_getInstanceMethod(UIViewController.self, sel) else { return }
        typealias Fn = @convention(c) (UIViewController, Selector) -> Void
        let original = unsafeBitCast(method_getImplementation(method), to: Fn.self)
        let block: @convention(block) (UIViewController) -> Void = { vc in
            original(vc, sel)
            MainActor.assumeIsolated { after(vc) }
        }
        method_setImplementation(method, imp_implementationWithBlock(block))
    }

    private static func swizzleAnimated(_ sel: Selector, after: @escaping @MainActor (UIViewController) -> Void) {
        guard let method = class_getInstanceMethod(UIViewController.self, sel) else { return }
        typealias Fn = @convention(c) (UIViewController, Selector, Bool) -> Void
        let original = unsafeBitCast(method_getImplementation(method), to: Fn.self)
        let block: @convention(block) (UIViewController, Bool) -> Void = { vc, animated in
            original(vc, sel, animated)
            MainActor.assumeIsolated { after(vc) }
        }
        method_setImplementation(method, imp_implementationWithBlock(block))
    }

    static func key(_ vc: UIViewController) -> String {
        "vc-" + String(UInt(bitPattern: ObjectIdentifier(vc).hashValue), radix: 16)
    }

    /// nil → not tracked.
    static func viewName(_ vc: UIViewController) -> String? {
        if let named = vc as? TrooperViewNaming { return named.trooperViewName }
        let cls: AnyClass = type(of: vc)
        let id = ObjectIdentifier(cls)
        if let cached = nameCache[id] { return cached }
        let className = NSStringFromClass(cls)
        var ancestors: [String] = []
        var sup: AnyClass? = class_getSuperclass(cls)
        while let s = sup, s != UIViewController.self, s != NSObject.self {
            ancestors.append(NSStringFromClass(s))
            sup = class_getSuperclass(s)
        }
        let isContainer = vc is UINavigationController || vc is UITabBarController
            || vc is UIPageViewController || vc is UISplitViewController
        let mainPath = Bundle.main.bundleURL.resolvingSymlinksInPath().path
        let isAppCode = Bundle(for: cls).bundleURL.resolvingSymlinksInPath().path.hasPrefix(mainPath)
        let name: String?
        if ViewNameFilter.shouldTrack(className: className, ancestors: ancestors, isContainer: isContainer, isAppCode: isAppCode, ignored: ignored) {
            name = ViewNameFilter.displayName(String(describing: cls))
        } else {
            name = nil
        }
        nameCache[id] = name
        return name
    }

    private static func didAppear(_ vc: UIViewController) {
        guard let name = viewName(vc) else { return }
        var loading: Double?
        if let start = objc_getAssociatedObject(vc, &loadTimeKey) as? NSNumber {
            loading = max(0, (ProcessInfo.processInfo.systemUptime - start.doubleValue) * 1000)
            objc_setAssociatedObject(vc, &loadTimeKey, nil, .OBJC_ASSOCIATION_RETAIN_NONATOMIC)  // first appearance only
        }
        TaskTrooper.startView(name, key: key(vc), loadingTimeMs: loading)
    }

    private static func didDisappear(_ vc: UIViewController) {
        guard viewName(vc) != nil else { return }
        TaskTrooper.stopView(key(vc))
    }
}

// MARK: - Display link helper

/// CADisplayLink needs an NSObject target; this forwards ticks to a closure.
@MainActor
final class DisplayLinkProxy: NSObject {
    private var link: CADisplayLink?
    private let onTick: @MainActor (CADisplayLink) -> Void

    init(onTick: @escaping @MainActor (CADisplayLink) -> Void) {
        self.onTick = onTick
        super.init()
    }

    func start() {
        guard link == nil else { return }
        let l = CADisplayLink(target: self, selector: #selector(tick(_:)))
        l.add(to: .main, forMode: .common)
        link = l
    }

    func stop() {
        link?.invalidate()
        link = nil
    }

    @objc private func tick(_ link: CADisplayLink) { onTick(link) }
}

// MARK: - Lifecycle: app start, frames, background flush

@MainActor
final class LifecycleObserver: NSObject {
    private let core: TrooperCore
    private let config: TrooperConfig
    private var launchPending: Bool
    private let processStartMs: Int64
    private let sdkStartMs: Int64
    private let prewarmed: Bool
    private var foregroundUptime: TimeInterval?
    private var firstFrameLink: DisplayLinkProxy?
    private var frameLink: DisplayLinkProxy?
    private var lastFrameTimestamp: CFTimeInterval = 0
    private var wasBackgrounded = false

    /// Launches slower than this are not reported (debugger, background launch).
    static let maxStartDurationMs: Double = 30_000

    init(core: TrooperCore, config: TrooperConfig) {
        self.core = core
        self.config = config
        self.processStartMs = tt_process_start_time_ms()
        self.sdkStartMs = core.clock.nowMs()
        self.prewarmed = ProcessInfo.processInfo.environment["ActivePrewarm"] == "1"
        self.launchPending = config.trackAppStart
        super.init()
    }

    func start() {
        let nc = NotificationCenter.default
        if AppEnvironment.isAppExtension {
            // Extensions get host lifecycle notifications instead of UIApplication's.
            nc.addObserver(self, selector: #selector(didBecomeActive), name: .NSExtensionHostDidBecomeActive, object: nil)
            nc.addObserver(self, selector: #selector(willResignActive), name: .NSExtensionHostWillResignActive, object: nil)
            nc.addObserver(self, selector: #selector(didEnterBackground), name: .NSExtensionHostDidEnterBackground, object: nil)
            nc.addObserver(self, selector: #selector(willEnterForeground), name: .NSExtensionHostWillEnterForeground, object: nil)
        } else {
            nc.addObserver(self, selector: #selector(didBecomeActive), name: UIApplication.didBecomeActiveNotification, object: nil)
            nc.addObserver(self, selector: #selector(willResignActive), name: UIApplication.willResignActiveNotification, object: nil)
            nc.addObserver(self, selector: #selector(didEnterBackground), name: UIApplication.didEnterBackgroundNotification, object: nil)
            nc.addObserver(self, selector: #selector(willEnterForeground), name: UIApplication.willEnterForegroundNotification, object: nil)
            nc.addObserver(self, selector: #selector(willTerminate), name: UIApplication.willTerminateNotification, object: nil)
        }

        // Extensions: no UIApplication; assume active (the extension is being shown).
        let state = AppEnvironment.sharedApplication?.applicationState ?? .active
        if state == .active {
            // SDK started after launch finished: measure up to the next frame if still plausible.
            didBecomeActive()
        }
        if state != .background { startFrameMonitor() }
    }

    @objc private func didBecomeActive() {
        if launchPending {
            launchPending = false
            awaitFirstFrame { [self] in reportLaunch() }
        } else if let t0 = foregroundUptime, config.trackAppStart {
            foregroundUptime = nil
            awaitFirstFrame { [self] in
                let ms = (ProcessInfo.processInfo.systemUptime - t0) * 1000
                reportAppStart(kind: "hot", durationMs: ms, reference: "foreground")
            }
        }
        TaskTrooper.hangDetector.current?.resume()
        startFrameMonitor()
    }

    @objc private func willResignActive() {
        TaskTrooper.hangDetector.current?.pause()
    }

    @objc private func willEnterForeground() {
        foregroundUptime = ProcessInfo.processInfo.systemUptime
        if wasBackgrounded {
            wasBackgrounded = false
            core.enterForeground()
        }
    }

    @objc private func didEnterBackground() {
        wasBackgrounded = true
        stopFrameMonitor()
        TaskTrooper.hangDetector.current?.pause()
        let core = self.core
        // performExpiringActivity is extension-safe (unlike UIApplication.beginBackgroundTask) and also
        // keeps an app alive in the background; the block must not return until the flush is done.
        ProcessInfo.processInfo.performExpiringActivity(withReason: "TaskTrooper.flush") { expired in
            guard !expired else { return }
            let done = DispatchSemaphore(value: 0)
            core.enterBackground { done.signal() }
            _ = done.wait(timeout: .now() + 25)
        }
    }

    @objc private func willTerminate() {
        core.flush()
    }

    private func reportLaunch() {
        let now = core.clock.nowMs()
        if prewarmed {
            reportAppStart(kind: "warm", durationMs: Double(now - sdkStartMs), reference: "sdk_init")
        } else if processStartMs > 0 {
            reportAppStart(kind: "cold", durationMs: Double(now - processStartMs), reference: "process")
        }
    }

    private func reportAppStart(kind: String, durationMs: Double, reference: String) {
        guard durationMs >= 0, durationMs <= Self.maxStartDurationMs else { return }
        let ts = kind == "cold" ? processStartMs : core.clock.nowMs() - Int64(durationMs)
        core.submit(.appStart, ts: ts) { e in
            e.appStart = RumAppStartPayload(kind: kind, durationMs: durationMs)
            e.attributes = ["start_reference": reference]
        }
    }

    private func awaitFirstFrame(_ done: @escaping @MainActor () -> Void) {
        firstFrameLink?.stop()
        let proxy = DisplayLinkProxy { [weak self] _ in
            self?.firstFrameLink?.stop()
            self?.firstFrameLink = nil
            done()
        }
        firstFrameLink = proxy
        proxy.start()
    }

    private func startFrameMonitor() {
        guard config.trackFrames, frameLink == nil else { return }
        lastFrameTimestamp = 0
        let proxy = DisplayLinkProxy { [weak self] link in self?.frame(link) }
        frameLink = proxy
        proxy.start()
    }

    private func stopFrameMonitor() {
        frameLink?.stop()
        frameLink = nil
    }

    private func frame(_ link: CADisplayLink) {
        defer { lastFrameTimestamp = link.timestamp }
        guard lastFrameTimestamp > 0 else { return }
        let duration = link.timestamp - lastFrameTimestamp
        core.frameStats.withLock { $0.record(duration: duration) }
    }
}

#endif
