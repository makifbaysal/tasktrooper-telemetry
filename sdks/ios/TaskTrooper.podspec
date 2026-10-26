Pod::Spec.new do |s|
  s.name             = 'TaskTrooper'
  s.version          = '0.1.0'
  s.summary          = 'TaskTrooper Telemetry RUM SDK for iOS and tvOS.'
  s.description      = 'Sessions, views, app start, network (traceparent), crashes, hangs and frame vitals sent to TaskTrooper Telemetry.'
  s.homepage         = 'https://github.com/tasktrooper/telemetry'
  s.license          = { :type => 'Apache-2.0' }
  s.author           = { 'TaskTrooper' => 'sdk@tasktrooper.dev' }
  s.source           = { :git => 'https://github.com/tasktrooper/telemetry.git', :tag => "ios-v#{s.version}" }
  s.module_name      = 'TaskTrooper'
  s.swift_versions   = ['5.9', '6.0']
  # Xcode 27 builds for iOS/tvOS 15+ only; `pod lib lint` fails below that. SPM keeps 13 (see README).
  s.ios.deployment_target  = '15.0'
  s.tvos.deployment_target = '15.0'
  s.default_subspec  = 'Core'

  # Paths are relative to the repository root (the podspec is published from sdks/ios).
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

  s.subspec 'SwiftUI' do |ui|
    ui.dependency   'TaskTrooper/Core'
    ui.source_files = "#{root}/Sources/TaskTrooperSwiftUI/**/*.swift"
    ui.frameworks   = 'SwiftUI'
  end
end
