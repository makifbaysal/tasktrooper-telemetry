require "json"

package = JSON.parse(File.read(File.join(__dir__, "package.json")))

Pod::Spec.new do |s|
  s.name         = "trooper-react-native"
  s.module_name  = "TrooperReactNative"
  s.version      = package["version"]
  s.summary      = package["description"]
  s.homepage     = "https://github.com/tasktrooper/telemetry"
  s.license      = package["license"]
  s.authors      = { "TaskTrooper" => "sdk@tasktrooper.dev" }
  s.platforms    = { :ios => "13.4", :tvos => "13.4" }
  s.source       = { :git => "https://github.com/tasktrooper/telemetry.git", :tag => "react-native-v#{s.version}" }
  s.source_files = "ios/**/*.{h,m,mm,swift}"
  s.private_header_files = "ios/**/*.h"
  s.swift_version = "5.9"

  # Optional: bridge to the TaskTrooper iOS SDK. The Swift helper uses `#if canImport(TaskTrooper)`, which only
  # succeeds when this pod can see the TaskTrooper module — set TROOPER_RN_NATIVE_SDK=1 for `pod install` to depend on it.
  if ENV["TROOPER_RN_NATIVE_SDK"] == "1"
    s.dependency "TaskTrooper/Core"
  end

  if respond_to?(:install_modules_dependencies, true)
    install_modules_dependencies(s)
  else
    s.dependency "React-Core"
  end
end
