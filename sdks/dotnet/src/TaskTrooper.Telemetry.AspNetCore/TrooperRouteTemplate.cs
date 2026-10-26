using System.Text;
using Microsoft.AspNetCore.Http;
using Microsoft.AspNetCore.Routing;
using Microsoft.AspNetCore.Routing.Patterns;

namespace TaskTrooper.Telemetry.AspNetCore;

/// <summary>Builds the normalized <c>trooper.route</c> (<c>/orders/:id</c>) from a matched endpoint.</summary>
public static class TrooperRouteTemplate
{
    /// <summary>
    /// Minimal APIs and attribute-routed controllers use the raw template; conventional MVC routes get
    /// their required values (<c>{controller}</c>, <c>{action}</c>, <c>{area}</c>) substituted.
    /// </summary>
    public static string? FromEndpoint(Endpoint? endpoint) =>
        endpoint is RouteEndpoint re ? FromPattern(re.RoutePattern) : null;

    public static string FromPattern(RoutePattern pattern)
    {
        var sb = new StringBuilder();
        foreach (var segment in pattern.PathSegments)
        {
            sb.Append('/');
            foreach (var part in segment.Parts)
            {
                switch (part)
                {
                    case RoutePatternLiteralPart literal:
                        sb.Append(literal.Content);
                        break;
                    case RoutePatternSeparatorPart separator:
                        sb.Append(separator.Content);
                        break;
                    case RoutePatternParameterPart parameter:
                        if (pattern.RequiredValues.TryGetValue(parameter.Name, out var required) &&
                            !ReferenceEquals(required, RoutePattern.RequiredValueAny) && required is string value && value.Length > 0)
                            sb.Append(value);
                        else
                            sb.Append(parameter.IsCatchAll ? '*' : ':').Append(parameter.Name);
                        break;
                }
            }
        }

        return sb.Length == 0 ? "/" : sb.ToString();
    }
}
