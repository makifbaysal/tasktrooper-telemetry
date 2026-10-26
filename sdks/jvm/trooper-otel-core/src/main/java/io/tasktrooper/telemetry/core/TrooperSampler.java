package io.tasktrooper.telemetry.core;

import io.opentelemetry.api.common.Attributes;
import io.opentelemetry.api.trace.Span;
import io.opentelemetry.api.trace.SpanContext;
import io.opentelemetry.api.trace.SpanKind;
import io.opentelemetry.api.trace.TraceState;
import io.opentelemetry.context.Context;
import io.opentelemetry.sdk.trace.data.LinkData;
import io.opentelemetry.sdk.trace.samplers.Sampler;
import io.opentelemetry.sdk.trace.samplers.SamplingDecision;
import io.opentelemetry.sdk.trace.samplers.SamplingResult;
import java.math.BigDecimal;
import java.util.List;

/**
 * Wraps a sampler and implements the sampling part of contracts.md §4:
 *
 * <ul>
 *   <li>root spans (no parent) write their rate into the W3C tracestate as {@code tt=r:<rate>}, so downstream services
 *       know the effective (upstream) rate of parent-based sampling;</li>
 *   <li>sampled entry spans (server/consumer spans whose parent is remote or absent) get {@code trooper.sample_rate}:
 *       the rate from the remote parent's {@code tt} tracestate member when present, else the configured rate. The
 *       writer scales RED counts by {@code 1/sample_rate}.</li>
 * </ul>
 */
public final class TrooperSampler implements Sampler {
    private final Sampler delegate;
    private final double rate;
    private final Attributes rateAttributes;
    private final String traceStateValue;

    private TrooperSampler(Sampler delegate, double rate) {
        this.delegate = delegate;
        this.rate = rate;
        this.rateAttributes = Attributes.of(TrooperAttributes.SAMPLE_RATE, rate);
        this.traceStateValue = formatTraceState(rate);
    }

    public static Sampler wrap(Sampler delegate, double rate) {
        if (delegate instanceof TrooperSampler) {
            return delegate;
        }
        return new TrooperSampler(delegate, rate);
    }

    /** {@code parentbased(traceidratio(rate))} with the trooper attribute. */
    public static Sampler create(double rate) {
        return wrap(Sampler.parentBased(Sampler.traceIdRatioBased(rate)), rate);
    }

    public double rate() {
        return rate;
    }

    @Override
    public SamplingResult shouldSample(
            Context parentContext, String traceId, String name, SpanKind spanKind, Attributes attributes, List<LinkData> parentLinks) {
        SamplingResult result = delegate.shouldSample(parentContext, traceId, name, spanKind, attributes, parentLinks);
        try {
            SpanContext parent = Span.fromContext(parentContext).getSpanContext();
            boolean root = !parent.isValid();
            if (root) {
                // Every root span advertises its rate, whatever the decision (unsampled contexts propagate too).
                Attributes attrs = result.getAttributes();
                if (result.getDecision() == SamplingDecision.RECORD_AND_SAMPLE && isEntryKind(spanKind)) {
                    attrs = merge(attrs, rateAttributes);
                }
                return new Result(result, attrs, traceStateValue);
            }
            if (result.getDecision() != SamplingDecision.RECORD_AND_SAMPLE || !isEntryKind(spanKind) || !parent.isRemote()) {
                return result;
            }
            Double upstream = parseTraceState(parent.getTraceState().get(TrooperAttributes.TRACESTATE_KEY));
            Attributes rateAttrs = upstream == null ? rateAttributes : Attributes.of(TrooperAttributes.SAMPLE_RATE, upstream);
            return new Result(result, merge(result.getAttributes(), rateAttrs), null);
        } catch (Throwable t) {
            SdkErrors.record("sampler", t);
            return result;
        }
    }

    private static boolean isEntryKind(SpanKind kind) {
        return kind == SpanKind.SERVER || kind == SpanKind.CONSUMER;
    }

    private static Attributes merge(Attributes base, Attributes extra) {
        return base.isEmpty() ? extra : base.toBuilder().putAll(extra).build();
    }

    /** {@code 0.25} → {@code r:0.25}; {@code 1.0} → {@code r:1}. Plain decimal, never scientific notation. */
    static String formatTraceState(double rate) {
        if (Double.isNaN(rate) || Double.isInfinite(rate)) {
            rate = 1.0;
        }
        double r = Math.max(0.0, Math.min(1.0, rate));
        String v = new BigDecimal(Double.toString(r)).stripTrailingZeros().toPlainString();
        return "r:" + v;
    }

    /** Parses the value of the {@code tt} tracestate member ({@code r:<rate>}, other {@code ;}-separated fields ignored). */
    static Double parseTraceState(String value) {
        if (value == null || value.isEmpty()) {
            return null;
        }
        for (String part : value.split(";")) {
            String p = part.trim();
            if (p.startsWith("r:")) {
                try {
                    double r = Double.parseDouble(p.substring(2));
                    if (r >= 0.0 && r <= 1.0) {
                        return r;
                    }
                } catch (NumberFormatException ignored) {
                    return null;
                }
            }
        }
        return null;
    }

    @Override
    public String getDescription() {
        return "TrooperSampler{" + delegate.getDescription() + ",rate=" + rate + "}";
    }

    @Override
    public String toString() {
        return getDescription();
    }

    private record Result(SamplingResult delegate, Attributes attributes, String traceStateValue) implements SamplingResult {
        @Override public SamplingDecision getDecision() { return delegate.getDecision(); }
        @Override public Attributes getAttributes() { return attributes; }
        @Override public TraceState getUpdatedTraceState(TraceState parentTraceState) {
            TraceState ts = delegate.getUpdatedTraceState(parentTraceState);
            if (traceStateValue == null) {
                return ts;
            }
            try {
                return ts.toBuilder().put(TrooperAttributes.TRACESTATE_KEY, traceStateValue).build();
            } catch (Throwable t) {
                SdkErrors.record("sampler", t);
                return ts;
            }
        }
    }
}
