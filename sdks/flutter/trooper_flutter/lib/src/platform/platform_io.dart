import 'dart:convert';
import 'dart:io';

import 'package:path_provider/path_provider.dart';

import '../replay/uploader.dart';
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

Future<ReplaySegmentStore> createReplaySegmentStore(int maxBytes) async {
  try {
    final base = await getApplicationSupportDirectory();
    final dir = Directory('${base.path}${Platform.pathSeparator}tasktrooper${Platform.pathSeparator}replay');
    await dir.create(recursive: true);
    return FileSegmentStore(dir, maxBytes);
  } catch (_) {
    return MemorySegmentStore(maxBytes: maxBytes);
  }
}

/// One gzipped segment per file (`<ts>-<seq>.seg`, written as `.tmp` and renamed). Segments that would exceed
/// [maxBytes] are dropped.
class FileSegmentStore implements ReplaySegmentStore {
  FileSegmentStore(this.dir, this.maxBytes) {
    try {
      for (final f in dir.listSync()) {
        if (f is File && f.path.endsWith('.tmp')) f.deleteSync();
      }
    } catch (_) {}
  }

  final Directory dir;
  final int maxBytes;
  int _seq = 0;
  int dropped = 0;

  List<File> _files() {
    try {
      final files = dir.listSync().whereType<File>().where((f) => f.path.endsWith('.seg')).toList()
        ..sort((a, b) => a.path.compareTo(b.path));
      return files;
    } catch (_) {
      return [];
    }
  }

  int _total(List<File> files) {
    var n = 0;
    for (final f in files) {
      try {
        n += f.lengthSync();
      } catch (_) {}
    }
    return n;
  }

  @override
  Future<bool> add(List<int> bytes) async {
    try {
      if (_total(_files()) + bytes.length > maxBytes) {
        dropped++;
        return false;
      }
      final ts = DateTime.now().millisecondsSinceEpoch.toString().padLeft(15, '0');
      final name = '$ts-${(_seq++ % 1000000).toString().padLeft(6, '0')}';
      final tmp = File('${dir.path}${Platform.pathSeparator}$name.tmp');
      await tmp.writeAsBytes(bytes, flush: true);
      await tmp.rename('${dir.path}${Platform.pathSeparator}$name.seg');
      return true;
    } catch (_) {
      return false;
    }
  }

  @override
  Future<StoredSegment?> oldest() async {
    for (final f in _files()) {
      try {
        return StoredSegment(f.path, await f.readAsBytes());
      } catch (_) {
        try {
          f.deleteSync();
        } catch (_) {}
      }
    }
    return null;
  }

  @override
  Future<void> remove(StoredSegment segment) async {
    try {
      await File(segment.id).delete();
    } catch (_) {}
  }

  @override
  Future<int> count() async => _files().length;
}
