import '../replay/uploader.dart';
import '../storage.dart';

Future<TrooperStorage> createPlatformStorage() async => MemoryStorage();

List<int>? gzipBytes(List<int> bytes) => null;

String platformOsName() => '';

String platformOsVersion() => '';

Future<ReplaySegmentStore> createReplaySegmentStore(int maxBytes) async => MemorySegmentStore(maxBytes: maxBytes);
