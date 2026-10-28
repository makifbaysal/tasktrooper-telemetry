using TaskTrooper.Telemetry.Profiling;

namespace TaskTrooper.Telemetry.Profiling.Tests;

public class FoldedProfileTests
{
    [Fact]
    public void Aggregates_same_stack_and_renders_endpoint_sections_after_unattributed_samples()
    {
        var p = new FoldedProfile(ProfileType.Cpu);
        p.Add(null, null, "Main;Work", 10);
        p.Add(null, null, "Main;Work", 5);
        p.Add("/v1/orders/:id", "GET", "Main;Handle;Load", 7);
        p.Add("/v1/orders/:id", "GET", "Main;Handle;Load", 3);
        p.Add("/v1/orders", "POST", "Main;Create", 4);
        p.Add("orders.created", null, "Main;Consume", 2);
        p.Add(null, null, "Main;Zero", 0);   // ignored
        p.Add(null, null, "Main;Neg", -1);   // ignored

        Assert.Equal(31, p.Total);
        Assert.Equal(4, p.StackCount);
        Assert.Equal(
            "Main;Work 15\n" +
            "# trooper.route=/v1/orders trooper.method=POST\n" +
            "Main;Create 4\n" +
            "# trooper.route=/v1/orders/:id trooper.method=GET\n" +
            "Main;Handle;Load 10\n" +
            "# trooper.route=orders.created\n" +
            "Main;Consume 2\n",
            p.Render());
    }

    [Fact]
    public void Folds_stacks_beyond_the_limit_into_other()
    {
        var p = new FoldedProfile(ProfileType.AllocSpace, maxStacks: 2);
        p.Add(null, null, "a", 1);
        p.Add(null, null, "b", 1);
        p.Add(null, null, "c", 5);
        p.Add(null, null, "d", 5);
        p.Add(null, null, "a", 1);
        Assert.Equal("[other] 10\na 2\nb 1\n", p.Render());
    }

    [Fact]
    public void Truncates_deep_stacks_on_the_root_side()
    {
        var frames = Enumerable.Range(0, 200).Select(i => "F" + i).ToList();
        var parts = FoldedProfile.Stack(frames).Split(';');
        Assert.Equal(FoldedProfile.MaxDepth, parts.Length);
        Assert.Equal(FoldedProfile.Truncated, parts[0]);
        Assert.Equal("F199", parts[^1]);
        Assert.Equal("a;b", FoldedProfile.Stack(new[] { "a", "b" }));
    }

    [Fact]
    public void Separators_are_removed_from_frames_and_labels()
    {
        Assert.Equal("a:b_c", FoldedProfile.Stack(new[] { "a;b c" }));
        var p = new FoldedProfile(ProfileType.Lock);
        p.Add("/x y", "GET\n", "s", 1);
        Assert.Equal("# trooper.route=/x_y trooper.method=GET_\ns 1\n", p.Render());
    }

    [Theory]
    [InlineData("Acme.Orders.OrderService.Load(int32,class System.String)", "Acme.Orders.OrderService.Load")]
    [InlineData("System.Collections.Generic.List`1[System.__Canon].AddWithResize(!0)", "System.Collections.Generic.List`1.AddWithResize")]
    [InlineData("Acme.Api.OrdersController+<GetAsync>d__4.MoveNext()", "Acme.Api.OrdersController.GetAsync")]
    [InlineData("Program+<>c.<Main>b__0_1()", "Program+<>c.<Main>b__0_1")]
    [InlineData("System.Threading.Thread.StartCallback()", "System.Threading.Thread.StartCallback")]
    [InlineData("", FrameNames.Unknown)]
    [InlineData(null, FrameNames.Unknown)]
    public void Normalizes_method_names(string? input, string expected) => Assert.Equal(expected, FrameNames.Method(input));

    [Fact]
    public void Unknown_frames_use_the_module_name()
    {
        Assert.Equal("[libcoreclr.so]", FrameNames.UnknownFrame("/usr/share/dotnet/shared/libcoreclr.so"));
        Assert.Equal("[native]", FrameNames.UnknownFrame(null));
    }

    [Theory]
    [InlineData("cpu,alloc_space,lock", new[] { ProfileType.Cpu, ProfileType.AllocSpace, ProfileType.Lock })]
    [InlineData(" WALL , inuse_space ", new[] { ProfileType.Wall })]
    public void Parses_type_lists(string input, ProfileType[] expected) =>
        Assert.Equal(expected.OrderBy(t => t), ProfileTypes.ParseList(input)!.OrderBy(t => t));

    [Fact]
    public void Unsupported_type_lists_parse_to_null() => Assert.Null(ProfileTypes.ParseList("inuse_space,goroutines"));
}
