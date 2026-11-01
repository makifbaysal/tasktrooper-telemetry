// RUM wire model. Mirrors proto/rum/v1/rum.proto; JSON keys are the proto
// snake_case field names, enums are serialized by value name.
//
// Default values (empty strings, 0, false, empty maps) are omitted, matching
// protojson semantics.

/// `trooper.rum.v1.EventType`.
enum RumEventType {
  sessionStart('SESSION_START'),
  viewStart('VIEW_START'),
  viewEnd('VIEW_END'),
  vital('VITAL'),
  error('ERROR'),
  resource('RESOURCE'),
  action('ACTION'),
  appStart('APP_START'),
  custom('CUSTOM');

  const RumEventType(this.wireName);

  /// Proto enum value name used on the wire.
  final String wireName;
}

void _put(Map<String, Object?> m, String key, Object? value) {
  if (value == null) return;
  if (value is String && value.isEmpty) return;
  if (value is bool && !value) return;
  if (value is num && value == 0) return;
  if (value is Map && value.isEmpty) return;
  m[key] = value;
}

/// One of the `RumEvent.payload` oneof members.
sealed class RumPayload {
  /// JSON key of this oneof member inside `RumEvent`.
  String get jsonField;
  Map<String, Object?> toJson();
}

class ViewPayload extends RumPayload {
  ViewPayload({
    this.url = '',
    this.referrer = '',
    this.loadingTimeMs = 0,
    this.timeSpentMs = 0,
    this.isInitial = false,
  });
  String url;
  String referrer;
  double loadingTimeMs;
  double timeSpentMs;
  bool isInitial;

  @override
  String get jsonField => 'view';

  @override
  Map<String, Object?> toJson() {
    final m = <String, Object?>{};
    _put(m, 'url', url);
    _put(m, 'referrer', referrer);
    _put(m, 'loading_time_ms', loadingTimeMs);
    _put(m, 'time_spent_ms', timeSpentMs);
    _put(m, 'is_initial', isInitial);
    return m;
  }
}

class VitalPayload extends RumPayload {
  VitalPayload({required this.name, required this.value, this.rating = ''});
  String name;
  double value;
  String rating;

  @override
  String get jsonField => 'vital';

  @override
  Map<String, Object?> toJson() {
    final m = <String, Object?>{'name': name};
    _put(m, 'value', value);
    _put(m, 'rating', rating);
    return m;
  }
}

class ErrorPayload extends RumPayload {
  ErrorPayload({
    this.type = '',
    this.message = '',
    this.stack = '',
    this.source = 'source',
    this.isCrash = false,
    this.thread = '',
    this.fingerprint = '',
  });
  String type;
  String message;
  String stack;

  /// source | network | console | crash | anr | promise
  String source;
  bool isCrash;
  String thread;
  String fingerprint;

  @override
  String get jsonField => 'error';

  @override
  Map<String, Object?> toJson() {
    final m = <String, Object?>{};
    _put(m, 'type', type);
    _put(m, 'message', message);
    _put(m, 'stack', stack);
    _put(m, 'source', source);
    _put(m, 'is_crash', isCrash);
    _put(m, 'thread', thread);
    _put(m, 'fingerprint', fingerprint);
    return m;
  }
}

class ResourcePayload extends RumPayload {
  ResourcePayload({
    required this.url,
    required this.method,
    this.statusCode = 0,
    this.durationMs = 0,
    this.requestBytes = 0,
    this.responseBytes = 0,
    this.traceId = '',
    this.spanId = '',
    this.initiator = 'http',
  });
  String url;
  String method;

  /// 0 = network error / aborted.
  int statusCode;
  double durationMs;
  int requestBytes;
  int responseBytes;
  String traceId;
  String spanId;
  String initiator;

  @override
  String get jsonField => 'resource';

  @override
  Map<String, Object?> toJson() {
    final m = <String, Object?>{};
    _put(m, 'url', url);
    _put(m, 'method', method);
    _put(m, 'status_code', statusCode);
    _put(m, 'duration_ms', durationMs);
    _put(m, 'request_bytes', requestBytes);
    _put(m, 'response_bytes', responseBytes);
    _put(m, 'trace_id', traceId);
    _put(m, 'span_id', spanId);
    _put(m, 'initiator', initiator);
    return m;
  }
}

class ActionPayload extends RumPayload {
  ActionPayload({required this.name, this.target = '', this.rage = false, this.dead = false});
  String name;
  String target;
  bool rage;
  bool dead;

  @override
  String get jsonField => 'action';

  @override
  Map<String, Object?> toJson() {
    final m = <String, Object?>{};
    _put(m, 'name', name);
    _put(m, 'target', target);
    _put(m, 'rage', rage);
    _put(m, 'dead', dead);
    return m;
  }
}

class AppStartPayload extends RumPayload {
  AppStartPayload({required this.kind, required this.durationMs});

  /// cold | warm | hot
  String kind;
  double durationMs;

  @override
  String get jsonField => 'app_start';

  @override
  Map<String, Object?> toJson() {
    final m = <String, Object?>{};
    _put(m, 'kind', kind);
    _put(m, 'duration_ms', durationMs);
    return m;
  }
}

class CustomPayload extends RumPayload {
  CustomPayload({required this.name, Map<String, String>? props, this.value = 0}) : props = props ?? {};
  String name;
  Map<String, String> props;
  double value;

  @override
  String get jsonField => 'custom';

  @override
  Map<String, Object?> toJson() {
    final m = <String, Object?>{};
    _put(m, 'name', name);
    _put(m, 'props', props);
    _put(m, 'value', value);
    return m;
  }
}

/// `trooper.rum.v1.User`. Only set after `identify()`.
class RumUser {
  RumUser({required this.id, Map<String, String>? traits}) : traits = traits ?? const {};
  final String id;
  final Map<String, String> traits;

  Map<String, Object?> toJson() {
    final m = <String, Object?>{};
    _put(m, 'id', id);
    _put(m, 'traits', traits);
    return m;
  }
}

/// `trooper.rum.v1.RumEvent`. Fields are mutable so `beforeSend` can mask them.
class RumEvent {
  RumEvent({
    required this.type,
    required this.tsMs,
    required this.sessionId,
    this.viewId = '',
    this.viewName = '',
    required this.anonymousId,
    this.user,
    this.connection = 'unknown',
    this.payload,
    Map<String, String>? attributes,
  }) : attributes = attributes ?? {};

  RumEventType type;
  int tsMs;
  String sessionId;
  String viewId;
  String viewName;
  String anonymousId;
  RumUser? user;
  String connection;
  RumPayload? payload;
  Map<String, String> attributes;

  Map<String, Object?> toJson() {
    final m = <String, Object?>{'type': type.wireName, 'ts_ms': tsMs};
    _put(m, 'session_id', sessionId);
    _put(m, 'view_id', viewId);
    _put(m, 'view_name', viewName);
    _put(m, 'anonymous_id', anonymousId);
    final u = user;
    if (u != null && u.id.isNotEmpty) m['user'] = u.toJson();
    _put(m, 'connection', connection);
    final p = payload;
    if (p != null) m[p.jsonField] = p.toJson();
    _put(m, 'attributes', attributes);
    return m;
  }
}

class RumDevice {
  RumDevice({
    this.type = '',
    this.manufacturer = '',
    this.model = '',
    this.locale = '',
    this.screenWidth = 0,
    this.screenHeight = 0,
  });
  String type;
  String manufacturer;
  String model;
  String locale;
  int screenWidth;
  int screenHeight;

  Map<String, Object?> toJson() {
    final m = <String, Object?>{};
    _put(m, 'type', type);
    _put(m, 'manufacturer', manufacturer);
    _put(m, 'model', model);
    _put(m, 'locale', locale);
    _put(m, 'screen_width', screenWidth);
    _put(m, 'screen_height', screenHeight);
    return m;
  }
}

/// `trooper.rum.v1.Resource` — constant per app process.
class RumResource {
  RumResource({
    required this.app,
    this.version = '',
    this.environment = '',
    this.platform = 'PLATFORM_FLUTTER',
    this.sdkName = 'trooper_flutter',
    this.sdkVersion = '',
    RumDevice? device,
    this.osName = '',
    this.osVersion = '',
    this.browserName = '',
    this.browserVersion = '',
    Map<String, String>? attributes,
  }) : device = device ?? RumDevice(),
       attributes = attributes ?? {};

  String app;
  String version;
  String environment;
  String platform;
  String sdkName;
  String sdkVersion;
  RumDevice device;
  String osName;
  String osVersion;
  String browserName;
  String browserVersion;
  Map<String, String> attributes;

  Map<String, Object?> toJson() {
    final m = <String, Object?>{};
    _put(m, 'app', app);
    _put(m, 'version', version);
    _put(m, 'environment', environment);
    _put(m, 'platform', platform);
    m['sdk'] = <String, Object?>{'name': sdkName, if (sdkVersion.isNotEmpty) 'version': sdkVersion};
    _put(m, 'device', device.toJson());
    final os = <String, Object?>{};
    _put(os, 'name', osName);
    _put(os, 'version', osVersion);
    _put(m, 'os', os);
    final browser = <String, Object?>{};
    _put(browser, 'name', browserName);
    _put(browser, 'version', browserVersion);
    _put(m, 'browser', browser);
    _put(m, 'attributes', attributes);
    return m;
  }
}

/// `trooper.rum.v1.RumBatch` JSON.
Map<String, Object?> rumBatchJson(Map<String, Object?> resource, List<Map<String, Object?>> events) => {
  'resource': resource,
  'events': events,
};
