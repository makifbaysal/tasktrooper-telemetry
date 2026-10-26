using System.Text.RegularExpressions;

namespace TaskTrooper.Telemetry;

/// <summary>Route template normalization: <c>api/orders/{id:int}</c> → <c>/api/orders/:id</c>.</summary>
public static partial class TrooperRoute
{
    [GeneratedRegex(@"\{\*{1,2}([^}:=?]+)[^}]*\}")]
    private static partial Regex CatchAll();

    [GeneratedRegex(@"\{([^}:=?]+)[^}]*\}")]
    private static partial Regex Parameter();

    /// <summary>Normalizes an ASP.NET-style template. Parameters become <c>:name</c>, catch-alls <c>*name</c>.</summary>
    public static string Normalize(string? template)
    {
        if (string.IsNullOrWhiteSpace(template)) return "/";
        var t = template.Trim();
        t = CatchAll().Replace(t, m => "*" + m.Groups[1].Value.Trim());
        t = Parameter().Replace(t, m => ":" + m.Groups[1].Value.Trim());
        if (!t.StartsWith('/')) t = "/" + t;
        if (t.Length > 1 && t.EndsWith('/')) t = t.TrimEnd('/');
        return t;
    }
}
