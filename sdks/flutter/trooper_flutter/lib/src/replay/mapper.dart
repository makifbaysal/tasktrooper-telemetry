import 'dart:math' as math;

import 'package:flutter/cupertino.dart';
import 'package:flutter/material.dart';
import 'package:flutter/rendering.dart';

import 'unmask.dart';
import 'wireframe.dart';

/// Result of one capture.
class ReplayCapture {
  const ReplayCapture(this.nodes, {required this.truncated, required this.elapsed, required this.elements});
  final List<WireNode> nodes;

  /// The node or time budget was hit; the frame contains what was found until then.
  final bool truncated;
  final Duration elapsed;

  /// Elements visited.
  final int elements;
}

enum _Role { button, input, switchLike, icon, image, list, webview }

class _Ctx {
  const _Ctx({
    required this.ro,
    required this.transform,
    required this.clip,
    required this.alpha,
    required this.unmask,
    this.role,
    this.roleWidget,
    this.button,
    this.input,
  });

  /// Nearest render object above (its [transform] maps its local coordinates to the view, logical px).
  final RenderObject? ro;
  final Matrix4 transform;

  /// Accumulated clip in view coordinates.
  final Rect clip;
  final double alpha;
  final bool unmask;

  /// Pending role of a widget above; consumed by the first render box below it.
  final _Role? role;
  final Widget? roleWidget;

  /// Button node above (its background is merged from the first Material / DecoratedBox below).
  final WireNode? button;

  /// Input node above (the EditableText below fills in its value).
  final WireNode? input;

  _Ctx copy({
    RenderObject? ro,
    Matrix4? transform,
    Rect? clip,
    double? alpha,
    bool? unmask,
    _Role? role,
    Widget? roleWidget,
    bool clearRole = false,
    WireNode? button,
    WireNode? input,
  }) => _Ctx(
    ro: ro ?? this.ro,
    transform: transform ?? this.transform,
    clip: clip ?? this.clip,
    alpha: alpha ?? this.alpha,
    unmask: unmask ?? this.unmask,
    role: clearRole ? null : (role ?? this.role),
    roleWidget: clearRole ? null : (roleWidget ?? this.roleWidget),
    button: button ?? this.button,
    input: input ?? this.input,
  );
}

class _Bg {
  _Bg(this.color, this.radius, this.border, {this.image = false});
  final int? color;
  final double radius;
  final int? border;
  final bool image;
}

/// Turns the Element tree into a flat, back-to-front wireframe (contracts.md §23, ADR-021).
///
/// * Widget type → kind (`Text`/`RichText` → text, `TextField`/`EditableText` → input, `Image` → image, `Icon` →
///   icon, Material buttons / `CupertinoButton` → button, `Switch`/`Checkbox`/`Radio` → switch, scroll views → list,
///   platform views → webview).
/// * Backgrounds from render objects: `Material`/`Card`/`PhysicalModel` (RenderPhysicalShape/Model), `Container`/
///   `DecoratedBox` (BoxDecoration / ShapeDecoration), `ColoredBox`.
/// * Geometry from each RenderBox (paint transform accumulated top-down, logical pixels), clipped by ancestors'
///   paint clips (scroll viewports, ClipRect…); `Offstage`, disabled `TickerMode` (offstage routes), opacity 0 and
///   fully clipped subtrees are skipped.
/// * Text is masked (length kept) unless inside `TrooperUnmask`; inputs are always masked; images are boxes.
///
/// No pixels are read. Bounded by [maxNodes] and [budget]. Main isolate, between frames.
class ReplayMapper {
  ReplayMapper({required this.maskAllText, this.maxNodes = 1500, this.budget = const Duration(milliseconds: 8)});

  final bool maskAllText;
  final int maxNodes;
  final Duration budget;

  late List<WireNode> _nodes;
  final Stopwatch _watch = Stopwatch();
  int _visits = 0;
  bool _truncated = false;
  int _budgetUs = 0;

  ReplayCapture capture(Element root, Size screen) {
    _nodes = <WireNode>[];
    _visits = 0;
    _truncated = false;
    _budgetUs = budget.inMicroseconds;
    _watch
      ..reset()
      ..start();
    try {
      _visit(
        root,
        _Ctx(ro: null, transform: Matrix4.identity(), clip: Offset.zero & screen, alpha: 1, unmask: !maskAllText),
        0,
      );
    } catch (_) {
      _truncated = true;
    }
    _watch.stop();
    return ReplayCapture(_nodes, truncated: _truncated, elapsed: _watch.elapsed, elements: _visits);
  }

  bool get _hasBudget {
    if (_truncated) return false;
    if (_nodes.length >= maxNodes) return !(_truncated = true);
    if ((++_visits & 31) == 0 && _watch.elapsedMicroseconds > _budgetUs) return !(_truncated = true);
    return true;
  }

  static const _maxDepth = 1000;

  void _visit(Element e, _Ctx ctx, int depth) {
    if (depth > _maxDepth || !_hasBudget) return;
    final w = e.widget;
    var c = ctx;
    if (w is Offstage && w.offstage) return;
    // Overlay entries below an opaque route (and hidden Visibility subtrees) have tickers disabled.
    if (w is TickerMode && !w.enabled) return;
    if (w is TrooperUnmask) c = c.copy(unmask: w.unmask);
    if (w is EditableText) {
      final input = c.input;
      if (input != null) {
        input.text = ReplayMasking.apply(w.controller.text, input: true, maskAll: maskAllText).$1;
        input.masked = input.text != null;
        return;
      }
      if (c.role == null) c = c.copy(role: _Role.input, roleWidget: w);
    } else if (c.role == null) {
      final role = _roleOf(w);
      if (role != null) c = c.copy(role: role, roleWidget: w);
    }
    if (e is RenderObjectElement) {
      final ro = e.renderObject;
      if (!identical(ro, c.ro)) {
        final next = _enter(ro, w, c);
        if (next == null) return;
        c = next;
      }
    }
    final child = c;
    e.visitChildren((el) => _visit(el, child, depth + 1));
  }

  static _Role? _roleOf(Widget w) {
    if (w is ButtonStyleButton ||
        w is IconButton ||
        w is FloatingActionButton ||
        w is MaterialButton ||
        w is RawMaterialButton ||
        w is CupertinoButton) {
      return _Role.button;
    }
    if (w is TextField || w is CupertinoTextField) return _Role.input;
    if (w is Switch ||
        w is Checkbox ||
        w is Radio ||
        w is CupertinoSwitch ||
        w is CupertinoCheckbox ||
        w is CupertinoRadio) {
      return _Role.switchLike;
    }
    if (w is Icon) return _Role.icon;
    if (w is Image || w is RawImage || w is FadeInImage) return _Role.image;
    if (w is ScrollView || w is SingleChildScrollView || w is PageView) return _Role.list;
    if (w is PlatformViewLink || w is AndroidView || w is UiKitView || w is HtmlElementView) return _Role.webview;
    return null;
  }

  /// Enters a render object: geometry, clip, opacity, nodes. Returns the context for its children, or null to
  /// skip the subtree.
  _Ctx? _enter(RenderObject ro, Widget w, _Ctx c) {
    try {
      final parent = c.ro;
      Matrix4 m;
      var clip = c.clip;
      if (ro is RenderView) {
        m = Matrix4.identity();
      } else if (parent != null && parent is! RenderView && identical(ro.parent, parent)) {
        m = c.transform.clone();
        parent.applyPaintTransform(ro, m);
        final pc = parent.describeApproximatePaintClip(ro);
        if (pc != null) clip = clip.intersect(MatrixUtils.transformRect(c.transform, pc));
      } else {
        if (!ro.attached) return null;
        m = ro.getTransformTo(null);
      }
      if (clip.width <= 0 || clip.height <= 0) return null;

      var alpha = c.alpha;
      if (ro is RenderOffstage && ro.offstage) return null;
      if (ro is RenderOpacity) {
        alpha *= ro.opacity;
      } else if (ro is RenderAnimatedOpacityMixin) {
        alpha *= ro.opacity.value;
      }
      if (alpha < 0.01) return null;

      var next = c.copy(ro: ro, transform: m, clip: clip, alpha: alpha);
      if (ro is! RenderBox) return next;
      if (!ro.hasSize) return null;
      final size = ro.size;
      if (size.isEmpty) return next;
      final rect = MatrixUtils.transformRect(m, Offset.zero & size);
      final visible = rect.intersect(clip);
      if (visible.width <= 0 || visible.height <= 0) return null;

      // A role widget above (Icon, TextField, ElevatedButton…): its first render box carries the node.
      final role = c.role;
      if (role != null) {
        next = next.copy(clearRole: true);
        final roleWidget = c.roleWidget;
        switch (role) {
          case _Role.icon:
            _add(ro, rect, visible, 'icon', alpha, fg: _iconColor(roleWidget, ro));
            return null;
          case _Role.image:
            _add(ro, rect, visible, 'image', alpha);
            return null;
          case _Role.webview:
            _add(ro, rect, visible, 'webview', alpha);
            return null;
          case _Role.switchLike:
            _add(ro, rect, visible, 'switch', alpha, bg: _switchColor(roleWidget), checked: _checkedOf(roleWidget));
            return null;
          case _Role.input:
            final node = _add(ro, rect, visible, 'input', alpha);
            if (roleWidget is EditableText) {
              if (roleWidget.readOnly) {
                // SelectableText & co.: displayed text, normal text masking rules.
                final (t, masked) = ReplayMasking.apply(
                  roleWidget.controller.text,
                  obscure: roleWidget.obscureText,
                  maskAll: maskAllText,
                  unmask: c.unmask,
                );
                node
                  ..kind = 'text'
                  ..text = t
                  ..masked = masked
                  ..fg = roleWidget.style.color?.toARGB32();
              } else {
                node
                  ..text = ReplayMasking.apply(roleWidget.controller.text, input: true, maskAll: maskAllText).$1
                  ..masked = node.text != null;
              }
              return null;
            }
            next = next.copy(input: node);
          case _Role.button:
            next = next.copy(button: _add(ro, rect, visible, 'button', alpha));
          case _Role.list:
            _add(ro, rect, visible, 'list', alpha);
        }
      }

      final bg = _background(ro, w, size);
      if (bg != null) {
        final button = next.button;
        if (button != null && button.bg == null && !bg.image) {
          // The visible shape of the button (inside its tap target) carries its color.
          button
            ..x = rect.left
            ..y = rect.top
            ..w = rect.width
            ..h = rect.height
            ..clip = _clipOf(rect, visible)
            ..bg = bg.color
            ..radius = bg.radius
            ..border = bg.border;
        } else if (bg.color != null || bg.border != null || bg.image) {
          _add(
            ro,
            rect,
            visible,
            bg.image ? 'image' : 'view',
            alpha,
            bg: bg.image ? null : bg.color,
            radius: bg.radius,
            border: bg.border,
          );
        }
      }

      if (ro is RenderParagraph) {
        final raw = ro.text.toPlainText(includeSemanticsLabels: false, includePlaceholders: false);
        final (t, masked) = ReplayMasking.apply(raw, maskAll: maskAllText, unmask: c.unmask);
        if (t != null) {
          _add(ro, rect, visible, 'text', alpha, fg: _spanColor(ro.text), text: t, masked: masked);
        }
      } else if (ro is RenderImage) {
        _add(ro, rect, visible, 'image', alpha);
        return null;
      }
      return next;
    } catch (_) {
      return null;
    }
  }

  static List<double>? _clipOf(Rect rect, Rect visible) {
    final clipped =
        visible.left > rect.left + 0.5 ||
        visible.top > rect.top + 0.5 ||
        visible.right < rect.right - 0.5 ||
        visible.bottom < rect.bottom - 0.5;
    return clipped ? [visible.left, visible.top, visible.width, visible.height] : null;
  }

  WireNode _add(
    RenderObject ro,
    Rect rect,
    Rect visible,
    String kind,
    double alpha, {
    int? bg,
    int? fg,
    double radius = 0,
    int? border,
    String? text,
    bool masked = false,
    bool? checked,
  }) {
    final node = WireNode(
      id: identityHashCode(ro),
      x: rect.left,
      y: rect.top,
      w: rect.width,
      h: rect.height,
      kind: kind,
      bg: bg,
      fg: fg,
      radius: radius,
      border: border,
      text: text,
      masked: masked,
      alpha: alpha,
      clip: _clipOf(rect, visible),
      checked: checked,
    );
    _nodes.add(node);
    return node;
  }

  // ───────────────────────── colors ─────────────────────────

  static _Bg? _background(RenderObject ro, Widget w, Size size) {
    if (w is ColoredBox) return _Bg(_opaqueOrNull(w.color), 0, null);
    if (ro is RenderPhysicalModel) {
      final radius = ro.shape == BoxShape.circle ? size.shortestSide / 2 : (ro.borderRadius?.topLeft.x ?? 0);
      return _Bg(_opaqueOrNull(ro.color), radius, null);
    }
    if (ro is RenderPhysicalShape) {
      final clipper = ro.clipper;
      final shape = clipper is ShapeBorderClipper ? clipper.shape : null;
      return _Bg(_opaqueOrNull(ro.color), _shapeRadius(shape, size), _shapeBorder(shape));
    }
    if (ro is RenderDecoratedBox) {
      final d = ro.decoration;
      if (d is BoxDecoration) {
        final radius = d.shape == BoxShape.circle
            ? size.shortestSide / 2
            : (d.borderRadius?.resolve(TextDirection.ltr).topLeft.x ?? 0);
        int? border;
        final b = d.border;
        if (b is Border) border = _side(b.top) ?? _side(b.bottom) ?? _side(b.left);
        return _Bg(_opaqueOrNull(d.color ?? d.gradient?.colors.firstOrNull), radius, border, image: d.image != null);
      }
      if (d is ShapeDecoration) {
        return _Bg(
          _opaqueOrNull(d.color ?? d.gradient?.colors.firstOrNull),
          _shapeRadius(d.shape, size),
          _shapeBorder(d.shape),
          image: d.image != null,
        );
      }
    }
    return null;
  }

  /// ARGB, or null when (nearly) fully transparent.
  static int? _opaqueOrNull(Color? c) {
    if (c == null) return null;
    final argb = c.toARGB32();
    return isTransparent(argb) ? null : argb;
  }

  static int? _side(BorderSide s) => s.style == BorderStyle.none || s.width <= 0 ? null : _opaqueOrNull(s.color);

  static double _shapeRadius(ShapeBorder? shape, Size size) {
    switch (shape) {
      case CircleBorder():
      case StadiumBorder():
        return size.shortestSide / 2;
      case RoundedRectangleBorder(:final borderRadius):
      case ContinuousRectangleBorder(:final borderRadius):
      case BeveledRectangleBorder(:final borderRadius):
        return math.min(borderRadius.resolve(TextDirection.ltr).topLeft.x, size.shortestSide / 2);
      case RoundedSuperellipseBorder(:final borderRadius):
        return math.min(borderRadius.resolve(TextDirection.ltr).topLeft.x, size.shortestSide / 2);
      default:
        return 0;
    }
  }

  static int? _shapeBorder(ShapeBorder? shape) => shape is OutlinedBorder ? _side(shape.side) : null;

  static int? _spanColor(InlineSpan span) {
    final c = span.style?.color;
    if (c != null) return c.toARGB32();
    int? found;
    span.visitChildren((s) {
      found = s.style?.color?.toARGB32();
      return found == null;
    });
    return found;
  }

  /// The icon glyph is a RichText below the Icon; its style has the resolved IconTheme color.
  static int? _iconColor(Widget? icon, RenderObject ro) {
    final explicit = icon is Icon ? icon.color : null;
    if (explicit != null) return explicit.toARGB32();
    int? found;
    void walk(RenderObject r, int depth) {
      if (found != null || depth > 8) return;
      if (r is RenderParagraph) {
        found = _spanColor(r.text);
        return;
      }
      r.visitChildren((child) => walk(child, depth + 1));
    }

    walk(ro, 0);
    return found;
  }

  /// `checked` for a `switch` node (tristate `null` = off). Radios only when they carry their own `groupValue`.
  static bool? _checkedOf(Widget? w) => switch (w) {
    Switch(:final value) || CupertinoSwitch(:final value) => value,
    Checkbox(:final value) || CupertinoCheckbox(:final value) => value ?? false,
    // ignore: deprecated_member_use
    Radio(:final value, :final groupValue) when groupValue != null => value == groupValue,
    // ignore: deprecated_member_use
    CupertinoRadio(:final value, :final groupValue) when groupValue != null => value == groupValue,
    _ => null,
  };

  static int? _switchColor(Widget? w) {
    Color? c;
    if (w is Switch) {
      c = w.value ? (w.activeTrackColor ?? w.activeThumbColor) : w.inactiveTrackColor;
    } else if (w is CupertinoSwitch) {
      c = w.value ? w.activeTrackColor : w.inactiveTrackColor;
    } else if (w is Checkbox) {
      c = w.value == true ? w.activeColor : null;
    }
    return _opaqueOrNull(c);
  }
}
