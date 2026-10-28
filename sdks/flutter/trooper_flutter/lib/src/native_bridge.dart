import 'package:flutter/services.dart';

import 'client.dart';

/// Optional hook to the native TaskTrooper Android/iOS SDKs running in the
/// same app (they capture native crashes, ANRs and OOMs that Dart cannot).
///
/// Channel `dev.tasktrooper.telemetry/rum`:
/// * Dart → native `setContext` `{session_id, anonymous_id, view_id, view_name, user_id}`
///   whenever any of them changes, so native crash reports join the Flutter session.
/// * Dart → native `getProcessStartTime` → epoch ms (int) of process start, for
///   APP_START. Optional.
/// * native → Dart `getContext` → the same map (for native code that starts
///   before Dart or needs the current values on demand).
///
/// Missing native side (no plugin) is silently ignored.
class TrooperNativeBridge {
  TrooperNativeBridge(this.client, {MethodChannel? channel}) : channel = channel ?? defaultChannel;

  static const defaultChannel = MethodChannel('dev.tasktrooper.telemetry/rum');

  final TrooperClient client;
  final MethodChannel channel;

  void attach() {
    try {
      channel.setMethodCallHandler((call) async {
        if (call.method == 'getContext') return client.context.toMap();
        throw MissingPluginException();
      });
      client.onContextChanged = setContext;
    } catch (_) {}
  }

  Future<void> setContext(TrooperContext context) async {
    try {
      await channel.invokeMethod<void>('setContext', context.toMap());
    } catch (_) {}
  }

  /// Whether the native SDK's own session replay runs in this app (Android `TrooperReplay.install`). Flutter
  /// replay is then not started (contracts.md §23 hybrid apps). False when unknown (no plugin, iOS).
  Future<bool> isNativeReplayActive() async {
    try {
      return await channel.invokeMethod<bool>('isNativeReplayActive') ?? false;
    } catch (_) {
      return false;
    }
  }

  Future<DateTime?> processStartTime() async {
    try {
      final ms = await channel.invokeMethod<int>('getProcessStartTime');
      return ms == null || ms <= 0 ? null : DateTime.fromMillisecondsSinceEpoch(ms);
    } catch (_) {
      return null;
    }
  }
}
