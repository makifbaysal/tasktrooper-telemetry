using System.Net;

namespace TaskTrooper.Telemetry;

/// <summary>
/// HTTP handler used by the OTLP exporter: retries 429/5xx and network errors with exponential backoff
/// (base→60s, full jitter, honours Retry-After), returns other 4xx immediately so the batch is dropped (contracts §2).
/// Runs on the exporter's background thread, never on request threads.
/// </summary>
internal sealed class TrooperRetryHandler : DelegatingHandler
{
    private static readonly TimeSpan MaxDelay = TimeSpan.FromSeconds(60);
    private readonly int maxRetries;
    private readonly TimeSpan baseDelay;
    private readonly TimeSpan attemptTimeout;

    public TrooperRetryHandler(HttpMessageHandler inner, int maxRetries, TimeSpan baseDelay, TimeSpan attemptTimeout)
        : base(inner)
    {
        this.maxRetries = Math.Max(0, maxRetries);
        this.baseDelay = baseDelay;
        this.attemptTimeout = attemptTimeout;
    }

    internal static bool IsRetryable(HttpStatusCode code) => code == HttpStatusCode.TooManyRequests || (int)code >= 500;

    protected override HttpResponseMessage Send(HttpRequestMessage request, CancellationToken cancellationToken)
    {
        request.Content?.LoadIntoBufferAsync().GetAwaiter().GetResult();
        for (var attempt = 0; ; attempt++)
        {
            HttpResponseMessage? response = null;
            try
            {
                using var cts = CancellationTokenSource.CreateLinkedTokenSource(cancellationToken);
                cts.CancelAfter(attemptTimeout);
                response = base.Send(request, cts.Token);
                if (!IsRetryable(response.StatusCode) || attempt >= maxRetries) return response;
            }
            catch (Exception e) when (e is HttpRequestException or TaskCanceledException && !cancellationToken.IsCancellationRequested)
            {
                if (attempt >= maxRetries) throw;
            }

            var delay = Delay(attempt, response);
            TrooperSelfDiagnostics.Retry((int?)response?.StatusCode ?? 0);
            response?.Dispose();
            if (cancellationToken.WaitHandle.WaitOne(delay)) cancellationToken.ThrowIfCancellationRequested();
        }
    }

    protected override async Task<HttpResponseMessage> SendAsync(HttpRequestMessage request, CancellationToken cancellationToken)
    {
        if (request.Content is not null) await request.Content.LoadIntoBufferAsync().ConfigureAwait(false);
        for (var attempt = 0; ; attempt++)
        {
            HttpResponseMessage? response = null;
            try
            {
                using var cts = CancellationTokenSource.CreateLinkedTokenSource(cancellationToken);
                cts.CancelAfter(attemptTimeout);
                response = await base.SendAsync(request, cts.Token).ConfigureAwait(false);
                if (!IsRetryable(response.StatusCode) || attempt >= maxRetries) return response;
            }
            catch (Exception e) when (e is HttpRequestException or TaskCanceledException && !cancellationToken.IsCancellationRequested)
            {
                if (attempt >= maxRetries) throw;
            }

            var delay = Delay(attempt, response);
            TrooperSelfDiagnostics.Retry((int?)response?.StatusCode ?? 0);
            response?.Dispose();
            await Task.Delay(delay, cancellationToken).ConfigureAwait(false);
        }
    }

    internal TimeSpan Delay(int attempt, HttpResponseMessage? response)
    {
        var retryAfter = response?.Headers.RetryAfter;
        if (retryAfter?.Delta is { } delta) return Min(delta, MaxDelay);
        if (retryAfter?.Date is { } date) return Min(Max(date - DateTimeOffset.UtcNow, TimeSpan.Zero), MaxDelay);

        var cap = Min(baseDelay * Math.Pow(2, Math.Min(attempt, 16)), MaxDelay);
        // "equal jitter": half fixed, half random → never zero, never above cap
        return cap / 2 + cap / 2 * Random.Shared.NextDouble();
    }

    private static TimeSpan Min(TimeSpan a, TimeSpan b) => a < b ? a : b;
    private static TimeSpan Max(TimeSpan a, TimeSpan b) => a > b ? a : b;
}
