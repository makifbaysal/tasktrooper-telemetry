using System.Text;

namespace TaskTrooper.Telemetry.Profiling;

/// <summary>
/// One profile of one type in the folded wire format (contracts §24):
/// <code>
/// frame1;frame2;…;frameN value          ← no endpoint
/// # trooper.route=/v1/orders/:id trooper.method=GET
/// frame1;…;frameN value                 ← samples taken while serving GET /v1/orders/:id
/// </code>
/// Root frame first, leaf last, value in the type's unit. More than <c>maxStacks</c> distinct stacks fold the rest
/// into <c>[other]</c>. Not thread-safe.
/// </summary>
public sealed class FoldedProfile
{
    public const int MaxDepth = 128;
    public const int DefaultMaxStacks = 10_000;
    public const string Truncated = "[truncated]";
    public const string Other = "[other]";

    private readonly int maxStacks;
    private readonly Dictionary<Section, Dictionary<string, long>> sections = new();

    public FoldedProfile(ProfileType type, int maxStacks = DefaultMaxStacks)
    {
        Type = type;
        this.maxStacks = Math.Max(1, maxStacks);
    }

    public ProfileType Type { get; }

    public long Total { get; private set; }

    public int StackCount { get; private set; }

    public bool IsEmpty => Total == 0;

    /// <summary>Adds <paramref name="value"/> to <paramref name="stack"/> under the endpoint (<paramref name="route"/> null = none). Values ≤ 0 are ignored.</summary>
    public void Add(string? route, string? method, string stack, long value)
    {
        if (value <= 0 || string.IsNullOrEmpty(stack)) return;
        var section = string.IsNullOrWhiteSpace(route) ? Section.None : new Section(Clean(route), string.IsNullOrWhiteSpace(method) ? null : Clean(method));
        if (!sections.TryGetValue(section, out var m)) sections[section] = m = new Dictionary<string, long>();
        if (!m.ContainsKey(stack))
        {
            if (StackCount >= maxStacks) stack = Other;
            if (!m.ContainsKey(stack)) StackCount++;
        }

        m[stack] = m.GetValueOrDefault(stack) + value;
        Total += value;
    }

    /// <summary>Folded text: samples without endpoint first, then one <c># trooper.route=…</c> section per endpoint.</summary>
    public string Render()
    {
        var sb = new StringBuilder(Math.Max(256, StackCount * 64));
        foreach (var (section, stacks) in sections
                     .OrderBy(kv => kv.Key.Route.Length == 0 ? 0 : 1)
                     .ThenBy(kv => kv.Key.Route, StringComparer.Ordinal)
                     .ThenBy(kv => kv.Key.Method ?? "", StringComparer.Ordinal))
        {
            if (stacks.Count == 0) continue;
            if (section.Route.Length > 0)
            {
                sb.Append("# trooper.route=").Append(section.Route);
                if (section.Method is not null) sb.Append(" trooper.method=").Append(section.Method);
                sb.Append('\n');
            }

            foreach (var (stack, value) in stacks.OrderBy(kv => kv.Key, StringComparer.Ordinal))
                sb.Append(stack).Append(' ').Append(value).Append('\n');
        }

        return sb.ToString();
    }

    /// <summary>
    /// Joins frames (root first) into a folded stack. Keeps the leaf-most frames; when more than <see cref="MaxDepth"/>,
    /// the root side becomes <c>[truncated]</c>.
    /// </summary>
    public static string Stack(IReadOnlyList<string> rootFirst)
    {
        var n = rootFirst.Count;
        var from = 0;
        var sb = new StringBuilder(n * 32);
        if (n > MaxDepth)
        {
            from = n - (MaxDepth - 1);
            sb.Append(Truncated);
        }

        for (var i = from; i < n; i++)
        {
            if (sb.Length > 0) sb.Append(';');
            sb.Append(Clean(rootFirst[i]));
        }

        return sb.ToString();
    }

    /// <summary>Frame / label text may not contain the separators of the format.</summary>
    internal static string Clean(string s)
    {
        if (s.AsSpan().IndexOfAny(";\n\r \t") < 0) return s;
        return s.Replace(';', ':').Replace('\n', ' ').Replace('\r', ' ').Replace(' ', '_').Replace('\t', '_');
    }

    private readonly record struct Section(string Route, string? Method)
    {
        public static readonly Section None = new("", null);
    }
}
