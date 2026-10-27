import 'dart:convert';

/// One wireframe node (contracts.md §23): logical-pixel screen coordinates, back-to-front order.
/// Colors are ARGB ints, serialized as `#rrggbb` (opaque) or `#rrggbbaa`.
class WireNode {
  WireNode({
    required this.id,
    required this.x,
    required this.y,
    required this.w,
    required this.h,
    required this.kind,
    this.bg,
    this.fg,
    this.radius = 0,
    this.border,
    this.text,
    this.masked = false,
    this.alpha = 1,
    this.clip,
    this.checked,
  });

  final int id;
  double x;
  double y;
  double w;
  double h;
  String kind;
  int? bg;
  int? fg;
  double radius;
  int? border;
  String? text;
  bool masked;
  final double alpha;

  /// Visible part `[x, y, w, h]` when clipped by an ancestor; null when fully visible.
  List<double>? clip;

  /// Toggle state, only for `switch` nodes; null when unknown.
  bool? checked;

  void writeTo(StringBuffer sb) {
    sb
      ..write('{"id":')
      ..write(id)
      ..write(',"x":')
      ..write(num1(x))
      ..write(',"y":')
      ..write(num1(y))
      ..write(',"w":')
      ..write(num1(w))
      ..write(',"h":')
      ..write(num1(h))
      ..write(',"kind":"')
      ..write(kind)
      ..write('"');
    final b = bg;
    if (b != null && !isTransparent(b)) sb.write(',"bg":"${hexColor(b)}"');
    final f = fg;
    if (f != null) sb.write(',"fg":"${hexColor(f | 0xff000000)}"');
    if (radius > 0) sb.write(',"radius":${num1(radius)}');
    final br = border;
    if (br != null) sb.write(',"border":"${hexColor(br | 0xff000000)}"');
    final t = text;
    if (t != null) sb.write(',"text":${jsonEncode(t)}');
    if (masked) sb.write(',"masked":true');
    if (alpha < 0.999) sb.write(',"alpha":${(alpha * 100).round() / 100}');
    final c = clip;
    if (c != null) sb.write(',"clip":[${c.map(num1).join(',')}]');
    final ch = checked;
    if (ch != null) sb.write(',"checked":$ch');
    sb.write('}');
  }

  Map<String, Object?> toJson() => jsonDecode(toString()) as Map<String, Object?>;

  @override
  String toString() {
    final sb = StringBuffer();
    writeTo(sb);
    return sb.toString();
  }
}

/// Rounds to one decimal and prints integers without a fraction (`12`, `12.5`).
String num1(double v) {
  if (!v.isFinite) return '0';
  final r = (v * 10).round();
  return r % 10 == 0 ? '${r ~/ 10}' : '${r / 10}';
}

/// `#rrggbb` when opaque, else `#rrggbbaa` (contracts.md §23).
String hexColor(int argb) {
  final a = (argb >> 24) & 0xff;
  final rgb = (argb & 0xffffff).toRadixString(16).padLeft(6, '0');
  return a == 0xff ? '#$rgb' : '#$rgb${a.toRadixString(16).padLeft(2, '0')}';
}

bool isTransparent(int argb) => (argb >> 24) & 0xff == 0;

/// Text masking: every non-whitespace character becomes `•`, so length and word shape are preserved.
abstract final class ReplayMasking {
  static const maxText = 300;

  static String mask(String s) {
    final n = s.length < maxText ? s.length : maxText;
    final sb = StringBuffer();
    for (var i = 0; i < n; i++) {
      final c = s.codeUnitAt(i);
      sb.writeCharCode(_isSpace(c) ? c : 0x2022);
    }
    return sb.toString();
  }

  static bool _isSpace(int c) => c == 0x20 || c == 0x09 || c == 0x0a || c == 0x0d || c == 0xa0;

  /// Returns `(text, masked)`. Inputs and obscured fields are always masked; other text when [maskAll] unless
  /// [unmask].
  static (String?, bool) apply(
    String? raw, {
    bool input = false,
    bool obscure = false,
    required bool maskAll,
    bool unmask = false,
  }) {
    if (raw == null || raw.isEmpty) return (null, false);
    if (input || obscure || (maskAll && !unmask)) return (mask(raw), true);
    return (raw.length <= maxText ? raw : raw.substring(0, maxText), false);
  }
}

/// Wireframe event JSON (contracts.md §23 `events[]`).
abstract final class WireEvents {
  static String screen(int ts, int width, int height, double density, String? name) {
    final n = (name == null || name.isEmpty) ? '' : ',"name":${jsonEncode(name)}';
    return '{"type":"screen","ts":$ts,"width":$width,"height":$height,"density":${(density * 100).round() / 100}$n}';
  }

  /// The `nodes` array only; also the key used to detect unchanged frames.
  static String nodes(List<WireNode> nodes) {
    final sb = StringBuffer('[');
    for (var i = 0; i < nodes.length; i++) {
      if (i > 0) sb.write(',');
      nodes[i].writeTo(sb);
    }
    sb.write(']');
    return sb.toString();
  }

  static String frame(int ts, String nodesJson) => '{"type":"frame","ts":$ts,"nodes":$nodesJson}';

  static String touch(int ts, double x, double y, String phase) =>
      '{"type":"touch","ts":$ts,"x":${num1(x)},"y":${num1(y)},"phase":"$phase"}';

  static String keyboard(int ts, bool visible) => '{"type":"keyboard","ts":$ts,"visible":$visible}';
}
