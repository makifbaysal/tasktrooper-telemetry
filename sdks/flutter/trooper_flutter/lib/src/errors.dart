import 'package:flutter/foundation.dart';

/// Signature used by the error hooks.
typedef ErrorSink = void Function(Object error, StackTrace? stack, {String? context});

/// Installs `FlutterError.onError` and `PlatformDispatcher.instance.onError`
/// handlers that report to [sink] and then call the previously installed
/// handlers (or Flutter's default presentation). Returns an uninstaller.
VoidCallback installErrorHandlers(ErrorSink sink) {
  final previousFlutter = FlutterError.onError;
  final dispatcher = PlatformDispatcher.instance;
  final previousPlatform = dispatcher.onError;

  void flutterHandler(FlutterErrorDetails details) {
    try {
      sink(details.exception, details.stack, context: details.context?.toDescription());
    } catch (_) {}
    if (previousFlutter != null) {
      previousFlutter(details);
    } else {
      FlutterError.presentError(details);
    }
  }

  bool platformHandler(Object error, StackTrace stack) {
    try {
      sink(error, stack, context: 'PlatformDispatcher.onError');
    } catch (_) {}
    return previousPlatform?.call(error, stack) ?? false;
  }

  FlutterError.onError = flutterHandler;
  dispatcher.onError = platformHandler;

  return () {
    if (FlutterError.onError == flutterHandler) FlutterError.onError = previousFlutter;
    if (dispatcher.onError == platformHandler) dispatcher.onError = previousPlatform;
  };
}
