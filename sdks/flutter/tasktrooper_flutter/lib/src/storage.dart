/// Persistence used by the SDK: the offline batch queue and small key/value
/// state (anonymous id). Implementations must never throw.
abstract class TrooperStorage {
  Future<List<String>> loadQueue();
  Future<void> saveQueue(List<String> batches);
  Future<String?> readValue(String key);
  Future<void> writeValue(String key, String value);
}

/// In-memory storage (web queue, tests, fallback when the file system fails).
class MemoryStorage implements TrooperStorage {
  List<String> queue = [];
  final Map<String, String> values = {};

  @override
  Future<List<String>> loadQueue() async => List.of(queue);

  @override
  Future<void> saveQueue(List<String> batches) async => queue = List.of(batches);

  @override
  Future<String?> readValue(String key) async => values[key];

  @override
  Future<void> writeValue(String key, String value) async => values[key] = value;
}
