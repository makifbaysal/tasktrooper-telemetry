import 'dart:math';

final Random _defaultRandom = () {
  try {
    return Random.secure();
  } catch (_) {
    return Random();
  }
}();

/// Lower-case hex string of [bytes] random bytes (never all zeros).
String randomHex(int bytes, [Random? random]) {
  final r = random ?? _defaultRandom;
  while (true) {
    final sb = StringBuffer();
    var nonZero = false;
    for (var i = 0; i < bytes; i++) {
      final b = r.nextInt(256);
      if (b != 0) nonZero = true;
      sb.write(b.toRadixString(16).padLeft(2, '0'));
    }
    if (nonZero) return sb.toString();
  }
}

/// Random RFC 4122 v4 UUID.
String uuidV4([Random? random]) {
  final r = random ?? _defaultRandom;
  final b = List<int>.generate(16, (_) => r.nextInt(256));
  b[6] = (b[6] & 0x0f) | 0x40;
  b[8] = (b[8] & 0x3f) | 0x80;
  final h = b.map((x) => x.toRadixString(16).padLeft(2, '0')).join();
  return '${h.substring(0, 8)}-${h.substring(8, 12)}-${h.substring(12, 16)}-${h.substring(16, 20)}-${h.substring(20)}';
}
