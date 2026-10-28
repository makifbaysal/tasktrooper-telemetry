namespace TaskTrooper.Telemetry.Profiling;

/// <summary>Profile types of the .NET profiler (contracts §24 <c>type</c> / <c>unit</c>).</summary>
public enum ProfileType
{
    /// <summary><c>cpu</c> (nanoseconds): EventPipe thread samples taken while the thread ran managed code.</summary>
    Cpu,

    /// <summary><c>wall</c> (nanoseconds): every thread sample, running or waiting. Opt-in.</summary>
    Wall,

    /// <summary><c>alloc_space</c> (bytes): <c>GCAllocationTick</c> (one event per ~100 KB allocated, weighted by the amount).</summary>
    AllocSpace,

    /// <summary><c>lock</c> (nanoseconds): monitor / lock contention (<c>ContentionStart</c> stack, <c>ContentionStop</c> duration).</summary>
    Lock,
}

public static class ProfileTypes
{
    /// <summary>Default types: <c>cpu,alloc_space,lock</c>.</summary>
    public static readonly IReadOnlySet<ProfileType> Defaults = new HashSet<ProfileType> { ProfileType.Cpu, ProfileType.AllocSpace, ProfileType.Lock };

    public static string WireName(this ProfileType type) => type switch
    {
        ProfileType.Cpu => "cpu",
        ProfileType.Wall => "wall",
        ProfileType.AllocSpace => "alloc_space",
        ProfileType.Lock => "lock",
        _ => type.ToString().ToLowerInvariant(),
    };

    public static string Unit(this ProfileType type) => type == ProfileType.AllocSpace ? "bytes" : "nanoseconds";

    /// <summary>Parses a wire name (<c>cpu</c>, <c>wall</c>, <c>alloc_space</c>, <c>lock</c>); other types are not supported on .NET.</summary>
    public static bool TryParse(string? name, out ProfileType type)
    {
        type = default;
        switch (name?.Trim().ToLowerInvariant())
        {
            case "cpu": type = ProfileType.Cpu; return true;
            case "wall": type = ProfileType.Wall; return true;
            case "alloc_space": type = ProfileType.AllocSpace; return true;
            case "lock": type = ProfileType.Lock; return true;
            default: return false;
        }
    }

    /// <summary>Comma separated list (<c>TROOPER_PROFILING_TYPES</c>); unknown names are ignored, null when nothing valid.</summary>
    public static HashSet<ProfileType>? ParseList(string? list)
    {
        if (string.IsNullOrWhiteSpace(list)) return null;
        var set = new HashSet<ProfileType>();
        foreach (var part in list.Split(',', StringSplitOptions.RemoveEmptyEntries | StringSplitOptions.TrimEntries))
            if (TryParse(part, out var t)) set.Add(t);
        return set.Count == 0 ? null : set;
    }
}
