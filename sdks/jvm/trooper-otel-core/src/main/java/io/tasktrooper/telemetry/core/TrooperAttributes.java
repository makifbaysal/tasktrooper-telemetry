package io.tasktrooper.telemetry.core;

import io.opentelemetry.api.common.AttributeKey;
import java.util.List;

/** Attribute keys from the TaskTrooper semantic conventions (contracts.md §4). */
public final class TrooperAttributes {
    public static final AttributeKey<String> ROUTE = AttributeKey.stringKey("trooper.route");
    public static final AttributeKey<String> ENTRY_ROUTE = AttributeKey.stringKey("trooper.entry.route");
    /** HTTP method of the entry span; {@code CONSUME} for consumers, {@code GRPC} for gRPC. */
    public static final AttributeKey<String> ENTRY_METHOD = AttributeKey.stringKey("trooper.entry.method");
    public static final AttributeKey<String> PROCESS = AttributeKey.stringKey("trooper.process");
    public static final AttributeKey<String> CALLER = AttributeKey.stringKey("trooper.caller");
    public static final AttributeKey<Long> CONSUMER_ATTEMPT = AttributeKey.longKey("trooper.consumer.attempt");
    public static final AttributeKey<Boolean> CONSUMER_DLQ = AttributeKey.booleanKey("trooper.consumer.dlq");
    public static final AttributeKey<Double> SAMPLE_RATE = AttributeKey.doubleKey("trooper.sample_rate");

    /** W3C baggage member carrying the calling service name. */
    public static final String BAGGAGE_SERVICE = "tt.svc";
    /** W3C tracestate member carrying the root span's sample rate: {@code tt=r:<rate>}. */
    public static final String TRACESTATE_KEY = "tt";
    /** Message header carrying the attempt number across retry topics / re-publishes (decimal string, 1 = first). */
    public static final String ATTEMPT_HEADER = "tt-attempt";
    /** RabbitMQ quorum queue redelivery count (0 on first delivery). */
    public static final String RABBIT_DELIVERY_COUNT_HEADER = "x-delivery-count";
    /** Entry method values for non-HTTP entries. */
    public static final String METHOD_CONSUME = "CONSUME";
    public static final String METHOD_GRPC = "GRPC";
    /** Spring Kafka delivery attempt header. */
    public static final String KAFKA_DELIVERY_ATTEMPT_HEADER = "kafka_deliveryAttempt";

    // OTel semantic convention keys we read (kept local to avoid a semconv dependency).
    public static final AttributeKey<String> HTTP_ROUTE = AttributeKey.stringKey("http.route");
    public static final AttributeKey<String> HTTP_REQUEST_METHOD = AttributeKey.stringKey("http.request.method");
    /** Pre-1.20 semconv name. */
    public static final AttributeKey<String> HTTP_METHOD_OLD = AttributeKey.stringKey("http.method");
    public static final AttributeKey<String> RPC_SYSTEM = AttributeKey.stringKey("rpc.system");
    public static final AttributeKey<String> RPC_SERVICE = AttributeKey.stringKey("rpc.service");
    public static final AttributeKey<String> RPC_METHOD = AttributeKey.stringKey("rpc.method");
    public static final AttributeKey<String> MESSAGING_SYSTEM = AttributeKey.stringKey("messaging.system");
    public static final AttributeKey<String> MESSAGING_DESTINATION = AttributeKey.stringKey("messaging.destination.name");
    public static final AttributeKey<String> MESSAGING_CONSUMER_GROUP = AttributeKey.stringKey("messaging.consumer.group.name");
    /** Pre-1.27 semconv name still emitted by some instrumentations. */
    public static final AttributeKey<String> MESSAGING_KAFKA_CONSUMER_GROUP = AttributeKey.stringKey("messaging.kafka.consumer.group");
    /** Captured by OTel when {@code otel.instrumentation.messaging.experimental.capture-headers=kafka_deliveryAttempt}. */
    public static final AttributeKey<List<String>> MESSAGING_HEADER_DELIVERY_ATTEMPT =
            AttributeKey.stringArrayKey("messaging.header." + KAFKA_DELIVERY_ATTEMPT_HEADER);
    /** Captured {@code tt-attempt} header ('-' → '_'; the v3 semconv preview keeps the raw name). */
    public static final List<AttributeKey<List<String>>> MESSAGING_HEADER_ATTEMPT = List.of(
            AttributeKey.stringArrayKey("messaging.header.tt_attempt"),
            AttributeKey.stringArrayKey("messaging.header." + ATTEMPT_HEADER));
    /** Captured RabbitMQ {@code x-delivery-count} header. */
    public static final List<AttributeKey<List<String>>> MESSAGING_HEADER_RABBIT_DELIVERY_COUNT = List.of(
            AttributeKey.stringArrayKey("messaging.header.x_delivery_count"),
            AttributeKey.stringArrayKey("messaging.header." + RABBIT_DELIVERY_COUNT_HEADER));
    /** Headers to capture on messaging spans (otel.instrumentation.messaging.experimental.capture-headers). */
    public static final String CAPTURED_HEADERS =
            KAFKA_DELIVERY_ATTEMPT_HEADER + "," + ATTEMPT_HEADER + "," + RABBIT_DELIVERY_COUNT_HEADER;

    private TrooperAttributes() {}
}
