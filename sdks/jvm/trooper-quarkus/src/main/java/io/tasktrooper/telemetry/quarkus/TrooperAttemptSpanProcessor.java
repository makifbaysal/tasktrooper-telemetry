package io.tasktrooper.telemetry.quarkus;

import io.opentelemetry.api.trace.SpanKind;
import io.opentelemetry.context.Context;
import io.opentelemetry.sdk.trace.ReadWriteSpan;
import io.opentelemetry.sdk.trace.ReadableSpan;
import io.opentelemetry.sdk.trace.SpanProcessor;
import io.tasktrooper.telemetry.core.SdkErrors;
import io.tasktrooper.telemetry.core.TrooperAttributes;

/**
 * Sets {@code trooper.consumer.attempt} on consumer spans from the attempt that {@link TrooperAttemptPropagator}
 * read out of the message headers when the parent context was extracted. Explicit values win. Never throws.
 */
public final class TrooperAttemptSpanProcessor implements SpanProcessor {

    @Override
    public void onStart(Context parentContext, ReadWriteSpan span) {
        try {
            if (span.getKind() != SpanKind.CONSUMER || span.getAttribute(TrooperAttributes.CONSUMER_ATTEMPT) != null) {
                return;
            }
            Long attempt = TrooperAttemptPropagator.attemptFrom(parentContext);
            if (attempt != null) {
                span.setAttribute(TrooperAttributes.CONSUMER_ATTEMPT, attempt);
            }
        } catch (Throwable t) {
            SdkErrors.record("quarkus_attempt", t);
        }
    }

    @Override
    public boolean isStartRequired() {
        return true;
    }

    @Override
    public void onEnd(ReadableSpan span) {}

    @Override
    public boolean isEndRequired() {
        return false;
    }
}
