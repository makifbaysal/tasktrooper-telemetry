using System.Text;
using System.Text.RegularExpressions;

namespace TaskTrooper.Telemetry.Profiling;

/// <summary>
/// .NET frame names for profiles: <c>Namespace.Type.Method</c> — no signature, IL offset or generic instantiation;
/// compiler-generated async state machines are folded back to their method (<c>Foo+&lt;GetAsync&gt;d__3.MoveNext</c> →
/// <c>Foo.GetAsync</c>). Frames without method information become <c>[module]</c> or <c>[native]</c>.
/// </summary>
public static partial class FrameNames
{
    public const string Native = "[native]";
    public const string Unknown = "[unknown]";

    [GeneratedRegex(@"\+<([^>]+)>d__\d+\.MoveNext$")]
    private static partial Regex AsyncStateMachine();

    /// <summary>Normalizes a TraceEvent method name such as <c>Ns.List`1[System.__Canon].Add(!0)</c>.</summary>
    public static string Method(string? fullName)
    {
        if (string.IsNullOrWhiteSpace(fullName)) return Unknown;
        var s = fullName.Trim();
        var paren = s.IndexOf('(');
        if (paren > 0) s = s[..paren];
        if (s.IndexOf('[') >= 0) s = StripBrackets(s);
        if (s.Contains(">d__", StringComparison.Ordinal)) s = AsyncStateMachine().Replace(s, ".$1");
        s = s.Trim();
        return s.Length == 0 ? Unknown : FoldedProfile.Clean(s);
    }

    /// <summary>Frame for an address without method information.</summary>
    public static string UnknownFrame(string? moduleName)
    {
        if (string.IsNullOrWhiteSpace(moduleName)) return Native;
        var m = moduleName.Trim();
        var slash = m.LastIndexOfAny(new[] { '/', '\\' });
        if (slash >= 0) m = m[(slash + 1)..];
        return "[" + FoldedProfile.Clean(m) + "]";
    }

    private static string StripBrackets(string s)
    {
        var sb = new StringBuilder(s.Length);
        var depth = 0;
        foreach (var c in s)
        {
            if (c == '[') depth++;
            else if (c == ']') depth = Math.Max(0, depth - 1);
            else if (depth == 0) sb.Append(c);
        }

        return sb.ToString();
    }
}
