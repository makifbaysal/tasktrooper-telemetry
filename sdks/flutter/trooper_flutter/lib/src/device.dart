import 'dart:async';
import 'dart:ui' as ui;

import 'package:device_info_plus/device_info_plus.dart';
import 'package:flutter/foundation.dart';

import 'config.dart';
import 'model.dart';
import 'platform/platform.dart';
import 'version.dart';

/// Resource from config + OS only (no plugin calls).
RumResource baseResource(TrooperConfig config) => RumResource(
  app: config.app,
  version: config.version,
  environment: config.environment,
  sdkVersion: sdkVersion,
  attributes: Map.of(config.attributes),
  osName: platformOsName(),
  osVersion: platformOsVersion(),
);

/// Builds the batch [RumResource] (app, device, os). Never throws; device
/// details that cannot be read are left empty.
Future<RumResource> collectResource(TrooperConfig config) async {
  final resource = baseResource(config);
  final device = resource.device;
  try {
    final dispatcher = ui.PlatformDispatcher.instance;
    device.locale = dispatcher.locale.toLanguageTag();
    final view = dispatcher.implicitView ?? (dispatcher.views.isEmpty ? null : dispatcher.views.first);
    if (view != null) {
      final logical = view.physicalSize / view.devicePixelRatio;
      device.screenWidth = logical.width.round();
      device.screenHeight = logical.height.round();
      final shortest = logical.shortestSide;
      device.type = _deviceType(shortest);
    }
  } catch (_) {}
  try {
    final info = DeviceInfoPlugin();
    if (kIsWeb) {
      final b = await info.webBrowserInfo.timeout(const Duration(seconds: 2));
      resource.browserName = b.browserName.name;
      resource.browserVersion = b.appVersion ?? '';
    } else if (defaultTargetPlatform == TargetPlatform.android) {
      final a = await info.androidInfo.timeout(const Duration(seconds: 2));
      device.manufacturer = a.manufacturer;
      device.model = a.model;
      resource.osVersion = a.version.release;
    } else if (defaultTargetPlatform == TargetPlatform.iOS) {
      final i = await info.iosInfo.timeout(const Duration(seconds: 2));
      device.manufacturer = 'Apple';
      device.model = i.utsname.machine;
      resource.osVersion = i.systemVersion;
    }
  } catch (_) {}
  return resource;
}

String _deviceType(double shortestSide) {
  if (kIsWeb) return shortestSide < 600 ? 'mobile' : 'desktop';
  switch (defaultTargetPlatform) {
    case TargetPlatform.android:
    case TargetPlatform.iOS:
    case TargetPlatform.fuchsia:
      return shortestSide >= 600 ? 'tablet' : 'mobile';
    case TargetPlatform.macOS:
    case TargetPlatform.windows:
    case TargetPlatform.linux:
      return 'desktop';
  }
}
