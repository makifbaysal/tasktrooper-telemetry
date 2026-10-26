#
# TaskTrooper Telemetry Flutter plugin (channel `dev.tasktrooper.telemetry/rum`, contracts.md §9).
#
# The default subspec has no dependency on the native TaskTrooper iOS SDK. To bridge the Flutter session
# to it (shared session for native crashes), add to the host app's ios/Podfile, inside `target 'Runner'`:
#
#   pod 'TaskTrooper', :git => 'https://github.com/tasktrooper/telemetry.git', :tag => 'ios-v0.1.0'
#   pod 'trooper_flutter/Native', :path => '.symlinks/plugins/trooper_flutter/ios'
#
# (or register a bridge at runtime with `TrooperFlutterPlugin.hybridBridge`, e.g. when TaskTrooper comes via SPM).
#
Pod::Spec.new do |s|
  s.name             = 'trooper_flutter'
  s.version          = '0.1.0'
  s.summary          = 'TaskTrooper Telemetry RUM SDK for Flutter — native bridge.'
  s.description      = <<-DESC
Bridges the Flutter RUM SDK to the native TaskTrooper iOS SDK (shared session) and reports process start time.
                       DESC
  s.homepage         = 'https://tasktrooper.dev'
  s.license          = { :type => 'Apache-2.0' }
  s.author           = { 'TaskTrooper' => 'sdk@tasktrooper.dev' }
  s.source           = { :path => '.' }
  s.platform         = :ios, '13.0'
  s.swift_version    = '5.0'
  s.default_subspec  = 'Core'

  # Flutter.framework does not contain a i386 slice.
  s.pod_target_xcconfig = { 'DEFINES_MODULE' => 'YES', 'EXCLUDED_ARCHS[sdk=iphonesimulator*]' => 'i386' }

  s.subspec 'Core' do |core|
    core.source_files     = 'Classes/**/*'
    core.dependency 'Flutter'
    core.resource_bundles = { 'trooper_flutter_privacy' => ['Resources/PrivacyInfo.xcprivacy'] }
  end

  # Opt-in: compiles the `#if canImport(TaskTrooper)` path against the native SDK.
  s.subspec 'Native' do |native|
    native.dependency 'trooper_flutter/Core'
    native.dependency 'TaskTrooper/Core'
  end
end
