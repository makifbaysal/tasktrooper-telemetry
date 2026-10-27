package io.tasktrooper.telemetry.core.profiling;

import static org.assertj.core.api.Assertions.assertThat;

import java.util.ArrayList;
import java.util.List;
import org.junit.jupiter.api.Test;

class FoldedProfileTest {

    @Test
    void rendersUnlabelledStacksFirstThenRouteSections() {
        FoldedProfile p = new FoldedProfile(ProfileType.CPU);
        p.add(null, null, "java.lang.Thread.run;com.acme.Worker.loop", 20);
        p.add("/v1/orders/:id", "GET", "java.lang.Thread.run;com.acme.Orders.get", 40);
        p.add("/v1/orders/:id", "GET", "java.lang.Thread.run;com.acme.Orders.get", 20);
        p.add("orders-consumer", "CONSUME", "java.lang.Thread.run;com.acme.Consumer.handle", 10);
        p.add(null, null, "java.lang.Thread.run;com.acme.Idle.zero", 0); // ignored

        assertThat(p.render()).isEqualTo("""
                java.lang.Thread.run;com.acme.Worker.loop 20
                # trooper.route=/v1/orders/:id trooper.method=GET
                java.lang.Thread.run;com.acme.Orders.get 60
                # trooper.route=orders-consumer trooper.method=CONSUME
                java.lang.Thread.run;com.acme.Consumer.handle 10
                """);
        assertThat(p.total()).isEqualTo(90);
        assertThat(p.stackCount()).isEqualTo(3);
    }

    @Test
    void sectionWithoutMethodOmitsIt() {
        FoldedProfile p = new FoldedProfile(ProfileType.LOCK);
        p.add("job", null, "a.B.c", 5);
        assertThat(p.render()).isEqualTo("# trooper.route=job\na.B.c 5\n");
    }

    @Test
    void stackIsRootFirstAndTruncatedOnTheRootSide() {
        assertThat(FoldedProfile.stack(List.of("root.A.main", "leaf.B.work"), false)).isEqualTo("root.A.main;leaf.B.work");
        // the recorder already cut the root side
        assertThat(FoldedProfile.stack(List.of("x.Y.a", "x.Y.b"), true)).isEqualTo("[truncated];x.Y.a;x.Y.b");

        List<String> deep = new ArrayList<>();
        for (int i = 0; i < 200; i++) deep.add("p.C.m" + i);
        String[] frames = FoldedProfile.stack(deep, false).split(";");
        assertThat(frames).hasSize(FoldedProfile.MAX_DEPTH);
        assertThat(frames[0]).isEqualTo("[truncated]");
        assertThat(frames[1]).isEqualTo("p.C.m73");
        assertThat(frames[frames.length - 1]).isEqualTo("p.C.m199"); // leaf kept
    }

    @Test
    void frameNamesAreStableAndSafe() {
        assertThat(FoldedProfile.frame("com.acme.Foo", "bar")).isEqualTo("com.acme.Foo.bar");
        assertThat(FoldedProfile.frame("com.acme.Foo$Inner", "run")).isEqualTo("com.acme.Foo$Inner.run");
        assertThat(FoldedProfile.frame("com.acme.Foo$$Lambda/0x000000080012c440", "apply")).isEqualTo("com.acme.Foo$$Lambda.apply");
        assertThat(FoldedProfile.frame("com.acme.Foo$$Lambda$123/0x0000000800c0b000", "apply")).isEqualTo("com.acme.Foo$$Lambda.apply");
        assertThat(FoldedProfile.frame("jdk.proxy2.$Proxy45", "invoke")).isEqualTo("jdk.proxy2.$Proxy.invoke");
        assertThat(FoldedProfile.frame("com.acme.Svc$$SpringCGLIB$$0", "call")).isEqualTo("com.acme.Svc$$SpringCGLIB.call");
        assertThat(FoldedProfile.frame("jdk.internal.reflect.GeneratedMethodAccessor12", "invoke"))
                .isEqualTo("jdk.internal.reflect.GeneratedMethodAccessor.invoke");
        // separators of the format never leak into frames
        assertThat(FoldedProfile.stack(List.of("a;b.c", "d e.f"), false)).isEqualTo("a:b.c;d_e.f");
    }

    @Test
    void tooManyDistinctStacksFoldIntoOther() {
        FoldedProfile p = new FoldedProfile(ProfileType.ALLOC_SPACE, 2);
        p.add(null, null, "a.A.a", 1);
        p.add(null, null, "b.B.b", 2);
        p.add(null, null, "c.C.c", 3);
        p.add(null, null, "d.D.d", 4);
        p.add(null, null, "a.A.a", 10); // known stack keeps its own line
        assertThat(p.render()).contains("a.A.a 11\n", "b.B.b 2\n", "[other] 7\n");
        assertThat(p.total()).isEqualTo(20);
    }

    @Test
    void parsesConfiguredTypes() {
        List<String> unsupported = new ArrayList<>();
        assertThat(ProfileType.parse(List.of("cpu", "inuse_space", "LOCK", "goroutines"), unsupported))
                .containsExactlyInAnyOrder(ProfileType.CPU, ProfileType.LOCK);
        assertThat(unsupported).containsExactly("inuse_space", "goroutines");
        assertThat(ProfileType.parse(List.of(), null)).containsExactlyInAnyOrder(ProfileType.values());
        assertThat(ProfileType.CPU.unit()).isEqualTo("nanoseconds");
        assertThat(ProfileType.ALLOC_SPACE.wireName()).isEqualTo("alloc_space");
        assertThat(ProfileType.ALLOC_SPACE.unit()).isEqualTo("bytes");
    }
}
