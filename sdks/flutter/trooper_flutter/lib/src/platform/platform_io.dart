import 'dart:convert';
import 'dart:io';

import 'package:path_provider/path_provider.dart';

import '../storage.dart';

Future<TrooperStorage> createPlatformStorage() async {
  try {
    final base = await getApplicationSupportDirectory();
    final dir = Directory('${base.path}${Platform.pathSeparator}tasktrooper');
    await dir.create(recursive: true);
    return FileStorage(dir);
  } catch (_) {
    return MemoryStorage();
  }
}

List<int>? gzipBytes(List<int> bytes) {
  try {
    return gzip.encode(bytes);
  } catch (_) {
    return null;
  }
}

String platformOsName() {
  switch (Platform.operatingSystem) {
    case 'android':
      return 'Android';
    case 'ios':
      return 'iOS';
    case 'macos':
      return 'macOS';
    case 'windows':
      return 'Windows';
    case 'linux':
      return 'Linux';
    default:
      return Platform.operatingSystem;
  }
}

String platformOsVersion() => Platform.operatingSystemVersion;

/// Queue + key/value state as files in the app support directory.
/// Writes go to a temp file and are renamed (atomic on the same volume).
class FileStorage implements TrooperStorage {
  FileStorage(this.dir);
  final Directory dir;

  File get _queueFile => File('${dir.path}${Platform.pathSeparator}rum_queue.jsonl');
  File get _stateFile => File('${dir.path}${Platform.pathSeparator}state.json');

  Future<void> _atomicWrite(File f, String content) async {
    final tmp = File('${f.path}.tmp');
    await tmp.writeAsString(content, flush: true);
    await tmp.rename(f.path);
  }

  @override
  Future<List<String>> loadQueue() async {
    try {
      if (!await _queueFile.exists()) return [];
      final lines = await _queueFile.readAsLines();
      return lines.where((l) => l.trim().isNotEmpty).toList();
    } catch (_) {
      return [];
    }
  }

  @override
  Future<void> saveQueue(List<String> batches) async {
    try {
      await _atomicWrite(_queueFile, batches.join('\n'));
    } catch (_) {}
  }

  Future<Map<String, Object?>> _readState() async {
    try {
      if (!await _stateFile.exists()) return {};
      final decoded = jsonDecode(await _stateFile.readAsString());
      return decoded is Map<String, Object?> ? decoded : {};
    } catch (_) {
      return {};
    }
  }

  @override
  Future<String?> readValue(String key) async {
    final v = (await _readState())[key];
    return v is String ? v : null;
  }

  @override
  Future<void> writeValue(String key, String value) async {
    try {
      final state = await _readState();
      state[key] = value;
      await _atomicWrite(_stateFile, jsonEncode(state));
    } catch (_) {}
  }
}
