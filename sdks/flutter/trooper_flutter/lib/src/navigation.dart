import 'package:flutter/scheduler.dart';
import 'package:flutter/widgets.dart';

import 'client.dart';
import 'task_trooper.dart';

/// Extracts the view name for a route; return null to ignore the route.
typedef ViewNameExtractor = String? Function(RouteSettings settings);

/// Reports every visible page route as a view (VIEW_START / VIEW_END).
///
/// The view name is `RouteSettings.name` — a route name such as `/products`
/// or, with go_router, the `GoRoute.name` (falls back to the path template
/// `/products/:id`). Unnamed routes and non-page routes (dialogs, bottom
/// sheets, popups) are ignored by default.
///
/// VIEW_START is emitted after the route's first frame and carries
/// `loading_time_ms` (push → first frame).
///
/// A NavigatorObserver can only be attached to one Navigator: create one
/// instance per navigator (e.g. per go_router `ShellRoute`).
class TrooperNavigatorObserver extends NavigatorObserver {
  TrooperNavigatorObserver({this.routeFilter = defaultRouteFilter, this.nameExtractor, TrooperClient? client})
    : _client = client;

  /// Which routes become views. Default: [PageRoute]s only.
  final bool Function(Route<dynamic> route) routeFilter;
  final ViewNameExtractor? nameExtractor;
  final TrooperClient? _client;

  Route<dynamic>? _current;

  static bool defaultRouteFilter(Route<dynamic> route) => route is PageRoute;

  TrooperClient? get _c => _client ?? TaskTrooper.client;

  String? _name(Route<dynamic> route) {
    final extractor = nameExtractor;
    final name = extractor != null ? extractor(route.settings) : route.settings.name;
    return (name == null || name.isEmpty) ? null : name;
  }

  void _start(Route<dynamic>? route) {
    try {
      final c = _c;
      if (c == null || route == null || !routeFilter(route)) return;
      final name = _name(route);
      if (name == null) return;
      _current = route;
      final id = c.startView(name, awaitFirstFrame: true);
      if (id.isEmpty) return;
      SchedulerBinding.instance
        ..addPostFrameCallback((_) => c.markViewLoaded(id))
        ..ensureVisualUpdate();
    } catch (_) {}
  }

  @override
  void didPush(Route<dynamic> route, Route<dynamic>? previousRoute) => _start(route);

  @override
  void didPop(Route<dynamic> route, Route<dynamic>? previousRoute) {
    if (identical(route, _current) || routeFilter(route)) _start(previousRoute);
  }

  @override
  void didReplace({Route<dynamic>? newRoute, Route<dynamic>? oldRoute}) {
    if (oldRoute == null || identical(oldRoute, _current) || routeFilter(oldRoute)) _start(newRoute);
  }

  @override
  void didRemove(Route<dynamic> route, Route<dynamic>? previousRoute) {
    if (identical(route, _current)) _start(previousRoute);
  }
}
