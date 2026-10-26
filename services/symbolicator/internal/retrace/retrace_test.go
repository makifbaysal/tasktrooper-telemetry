package retrace

import (
	"os"
	"strings"
	"testing"
)

func load(t *testing.T) *Mapping {
	t.Helper()
	f, err := os.Open("../../testdata/android/mapping.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	m, err := Parse(f)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// testdata/android/mapping.txt is real R8 8.11 output for testdata/android/src (see README).
func TestRetraceR8Fixture(t *testing.T) {
	m := load(t)
	if m.ID != "c6ba1e1" {
		t.Errorf("pg_map_id = %q", m.ID)
	}
	stack, _ := os.ReadFile("../../testdata/android/stack.txt")
	res := Retrace(string(stack), m)
	want := []struct {
		fn, file string
		line     int
		inlined  bool
	}{
		{"com.example.shop.PriceValidator.fail", "PriceValidator.java", 12, false},
		{"com.example.shop.PriceValidator.validate", "PriceValidator.java", 6, true},
		{"com.example.shop.CartService.computeTotal", "CartService.java", 17, true},
		{"com.example.shop.CartService.checkout", "CartService.java", 7, true},
		{"com.example.shop.Main.main", "Main.java", 7, false},
	}
	if len(res.Frames) != len(want) {
		t.Fatalf("frames = %d:\n%s", len(res.Frames), res.Stack)
	}
	for i, w := range want {
		f := res.Frames[i]
		if f.Fn != w.fn || f.File != w.file || f.Line != w.line || f.Inlined != w.inlined || !f.InApp {
			t.Errorf("frame %d = %+v, want %+v", i, f, w)
		}
	}
	if !strings.HasPrefix(res.Stack, "java.lang.IllegalStateException: negative price: -3\n\tat com.example.shop.PriceValidator.fail(PriceValidator.java:12)") {
		t.Errorf("stack:\n%s", res.Stack)
	}
}

const handMapping = `# compiler: R8
com.example.app.Repo -> b.c:
# {"id":"sourceFile","fileName":"Repo.kt"}
    java.lang.String name -> a
    1:5:void load(int):10:14 -> a
    6:6:void helper():40:40 -> a
    6:6:void load(int):15 -> a
    void plain() -> b
    void other(java.lang.String) -> b
    7:7:void save():50 -> c
      # {"id":"com.android.tools.r8.rewriteFrame","conditions":["throws(Ljava/lang/NullPointerException;)"],"actions":["removeInnerFrames(1)"]}
    7:7:void store():60 -> c
com.example.app.Repo$Inner -> b.d:
    void run() -> run
com.example.app.Boom -> b.e:
`

func TestRetraceCases(t *testing.T) {
	m, err := Parse(strings.NewReader(handMapping))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		in   string
		want []string // expected output lines
	}{
		{"range offset", "b.e: x\n\tat b.c.a(SourceFile:3)", []string{"com.example.app.Boom: x", "\tat com.example.app.Repo.load(Repo.kt:12)"}},
		{"inline", "b.e\n\tat b.c.a(SourceFile:6)", []string{"com.example.app.Boom", "\tat com.example.app.Repo.helper(Repo.kt:40)", "\tat com.example.app.Repo.load(Repo.kt:15)"}},
		{"ambiguous", "b.e\n\tat b.c.b(Unknown Source)", []string{"com.example.app.Boom", "\tat com.example.app.Repo.plain(Repo.kt)", "\t<OR> at com.example.app.Repo.other(Repo.kt)"}},
		{"rewriteFrame NPE", "java.lang.NullPointerException\n\tat b.c.c(SourceFile:7)", []string{"java.lang.NullPointerException", "\tat com.example.app.Repo.store(Repo.kt:60)"}},
		{"rewriteFrame other exception", "java.lang.IllegalStateException\n\tat b.c.c(SourceFile:7)", []string{"java.lang.IllegalStateException", "\tat com.example.app.Repo.save(Repo.kt:50)", "\tat com.example.app.Repo.store(Repo.kt:60)"}},
		{"inner class file", "Caused by: b.e: y\n\tat b.d.run(SourceFile)\n\t... 3 more", []string{"Caused by: com.example.app.Boom: y", "\tat com.example.app.Repo$Inner.run(Repo.kt)", "\t... 3 more"}},
		{"unknown class kept", "java.lang.Error\n\tat android.os.Handler.dispatch(Handler.java:99)", []string{"java.lang.Error", "\tat android.os.Handler.dispatch(Handler.java:99)"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := Retrace(tc.in, m).Stack
			if got != strings.Join(tc.want, "\n") {
				t.Errorf("got:\n%s\nwant:\n%s", got, strings.Join(tc.want, "\n"))
			}
		})
	}
}

func TestParseBadInput(t *testing.T) {
	for _, in := range []string{"", "garbage\nmore garbage", "    1:2:void x() -> a\n", strings.Repeat("x", 2<<20)} {
		if _, err := Parse(strings.NewReader(in)); err == nil {
			t.Errorf("Parse(%.20q) = nil error", in)
		}
	}
}
