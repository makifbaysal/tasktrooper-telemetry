namespace TaskTrooper.Telemetry;

/// <summary>
/// Settings for the TaskTrooper distribution. Resolution order (later wins):
/// environment variables → configuration section <c>TaskTrooper</c> → code delegate.
/// </summary>
public sealed class TaskTrooperOptions
{
    public const string SectionName = "TaskTrooper";

    /// <summary>DSN <c>https://&lt;key&gt;@&lt;host&gt;/&lt;project&gt;</c>. Env: <c>TROOPER_DSN</c>. Without a DSN nothing is exported.</summary>
    public string? Dsn { get; set; }

    /// <summary><c>service.name</c>. Env: <c>TROOPER_SERVICE</c> or <c>OTEL_SERVICE_NAME</c>. Default: entry assembly name.</summary>
    public string? ServiceName { get; set; }

    /// <summary><c>service.version</c> (deploy marker). Env: <c>TROOPER_VERSION</c>. Default: entry assembly informational version.</summary>
    public string? ServiceVersion { get; set; }

    /// <summary><c>deployment.environment.name</c>. Env: <c>TROOPER_ENVIRONMENT</c>. Default: host environment (lower-case).</summary>
    public string? Environment { get; set; }

    /// <summary>Head sampling probability 0..1 for new traces. Env: <c>TROOPER_SAMPLE_RATE</c>. Default 1.</summary>
    public double SampleRate { get; set; } = 1.0;

    /// <summary>Master switch. Env: <c>TROOPER_ENABLED</c>.</summary>
    public bool Enabled { get; set; } = true;

    public bool EnableTraces { get; set; } = true;
    public bool EnableMetrics { get; set; } = true;
    public bool EnableLogs { get; set; } = true;

    /// <summary>Per-request export timeout.</summary>
    public TimeSpan ExportTimeout { get; set; } = TimeSpan.FromSeconds(10);

    /// <summary>Retries on 429/5xx/network errors (exponential 1s→60s with jitter, honours Retry-After).</summary>
    public int MaxExportRetries { get; set; } = 5;

    /// <summary>
    /// Listen to GenAI ActivitySources (Microsoft.Extensions.AI, OpenAI .NET SDK, Azure AI Inference, Semantic Kernel) and
    /// turn on the OpenAI SDK's OpenTelemetry switch. Default <c>true</c>.
    /// </summary>
    public bool EnableLlm { get; set; } = true;

    /// <summary>
    /// Record LLM prompts / completions. Env: <c>TROOPER_LLM_CAPTURE_CONTENT</c>. <c>true</c>: on (also the default of
    /// M.E.AI <c>EnableSensitiveData</c>); <c>false</c>: content tags are stripped from every GenAI span; <c>null</c>
    /// (default): libraries' own opt-ins (all off by default) decide.
    /// </summary>
    public bool? LlmCaptureContent { get; set; }

    /// <summary>Extra resource attributes.</summary>
    public IDictionary<string, object> ResourceAttributes { get; } = new Dictionary<string, object>();

    /// <summary>Extra ActivitySource names to listen to (wildcards allowed).</summary>
    public IList<string> Sources { get; } = new List<string>();

    /// <summary>Extra Meter names to listen to (wildcards allowed).</summary>
    public IList<string> Meters { get; } = new List<string>();

    // test hook: base delay of the export retry backoff
    internal TimeSpan RetryBaseDelay { get; set; } = TimeSpan.FromSeconds(1);

    internal TrooperDsn? ParsedDsn => TrooperDsn.TryParse(Dsn, out var d) ? d : null;
}
