Pod::Spec.new do |s|
  s.name             = 'TaskTrooper'
  s.version          = '0.1.0'
  s.summary          = 'TaskTrooper Telemetry RUM SDK for iOS and tvOS.'
  s.description      = 'Sessions, views, app start, network (traceparent), crashes, hangs and frame vitals sent to TaskTrooper Telemetry.'
  s.homepage         = 'https://telemetry.tasktrooper.ai'
  s.license          = { :type => 'Apache-2.0', :file => 'sdks/LICENSE' }
  s.author           = 'Mehmet Akif Baysal'
  s.source           = { :git => 'https://github.com/makifbaysal/tasktrooper-telemetry.git', :tag => "ios-v#{s.version}" }
  s.module_name      = 'TaskTrooper'
  s.swift_versions   = ['5.9', '6.0']
  # Xcode 27 builds for iOS/tvOS 15+ only; `pod lib lint` fails below that. SPM keeps 13 (see README).
  s.ios.deployment_target  = '15.0'
  s.tvos.deployment_target = '15.0'
  s.default_subspec  = 'Core'

  # Paths are relative to the repository root: the pod is fetched from the monorepo at tag ios-v<version>.
  # The podspec file stays in sdks/ios (`pod trunk push sdks/ios/TaskTrooper.podspec`). `pod lib lint` resolves
  # paths from the podspec's directory, so lint a copy placed at the repository root (see README § Releasing).
  root = 'sdks/ios'

  s.subspec 'Core' do |core|
    core.source_files        = "#{root}/Sources/TaskTrooper/**/*.swift",
                               "#{root}/Sources/TaskTrooperCrashC/**/*.{h,c,m}"
    core.public_header_files = "#{root}/Sources/TaskTrooperCrashC/include/*.h"
    core.libraries           = 'z'
    core.frameworks          = 'Foundation', 'UIKit', 'Network'
    # Extension-safe: may be linked into app extensions.
    core.pod_target_xcconfig = { 'APPLICATION_EXTENSION_API_ONLY' => 'YES' }
    core.ios.weak_frameworks = 'MetricKit'
  end

  # Wireframe session replay (contracts.md §23). `.trooperUnmask()` for SwiftUI is included. Not extension-safe
  # (uses UIApplication); install is a no-op in app extensions and on tvOS.
  s.subspec 'Replay' do |replay|
    replay.dependency   'TaskTrooper/Core'
    replay.source_files = "#{root}/Sources/TaskTrooperReplay/**/*.swift"
    replay.frameworks   = 'UIKit', 'SwiftUI'
  end

  s.subspec 'SwiftUI' do |ui|
    ui.dependency   'TaskTrooper/Core'
    ui.source_files = "#{root}/Sources/TaskTrooperSwiftUI/**/*.swift"
    ui.frameworks   = 'SwiftUI'
  end
end
