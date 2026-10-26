import 'package:flutter/foundation.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:trooper_flutter/trooper_flutter.dart';

void main() {
  test('FlutterError.onError is chained to the previous handler', () {
    final original = FlutterError.onError;
    final previousCalls = <FlutterErrorDetails>[];
    FlutterError.onError = previousCalls.add;
    final captured = <Object>[];
    final uninstall = installErrorHandlers((e, s, {context}) => captured.add(e));
    try {
      final details = FlutterErrorDetails(exception: StateError('build failed'), stack: StackTrace.current);
      FlutterError.onError!(details);
      expect(captured.single, isA<StateError>());
      expect(previousCalls.single, same(details));
    } finally {
      uninstall();
      FlutterError.onError = original;
    }
  });

  test('PlatformDispatcher.onError is chained and keeps its return value', () {
    final dispatcher = PlatformDispatcher.instance;
    final original = dispatcher.onError;
    dispatcher.onError = (e, s) => true;
    final captured = <Object>[];
    final uninstall = installErrorHandlers((e, s, {context}) => captured.add(e));
    try {
      expect(dispatcher.onError!(Exception('async'), StackTrace.current), isTrue);
      expect(captured.single, isA<Exception>());
    } finally {
      uninstall();
      dispatcher.onError = original;
    }
  });

  test('sink errors never break the chain', () {
    final original = FlutterError.onError;
    var previousCalled = false;
    FlutterError.onError = (_) => previousCalled = true;
    final uninstall = installErrorHandlers((e, s, {context}) => throw Exception('sdk bug'));
    try {
      FlutterError.onError!(FlutterErrorDetails(exception: Exception('x')));
      expect(previousCalled, isTrue);
    } finally {
      uninstall();
      FlutterError.onError = original;
    }
  });
}
