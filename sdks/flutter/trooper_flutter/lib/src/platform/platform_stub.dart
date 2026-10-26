import '../storage.dart';

Future<TrooperStorage> createPlatformStorage() async => MemoryStorage();

List<int>? gzipBytes(List<int> bytes) => null;

String platformOsName() => '';

String platformOsVersion() => '';
