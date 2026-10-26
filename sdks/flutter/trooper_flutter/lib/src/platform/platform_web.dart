import 'package:web/web.dart' as web;

import '../storage.dart';

/// Web: the offline queue stays in memory (lost on reload); key/value state
/// (anonymous id) lives in `localStorage`.
Future<TrooperStorage> createPlatformStorage() async => _WebStorage();

/// Browsers decompress transparently but cannot set Content-Encoding on a
/// compressed body portably, so web sends plain JSON.
List<int>? gzipBytes(List<int> bytes) => null;

String platformOsName() => '';

String platformOsVersion() => '';

class _WebStorage extends MemoryStorage {
  static const _prefix = 'tasktrooper.';

  @override
  Future<String?> readValue(String key) async {
    try {
      return web.window.localStorage.getItem('$_prefix$key');
    } catch (_) {
      return super.readValue(key);
    }
  }

  @override
  Future<void> writeValue(String key, String value) async {
    try {
      web.window.localStorage.setItem('$_prefix$key', value);
    } catch (_) {
      await super.writeValue(key, value);
    }
  }
}
