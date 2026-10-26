package native

import (
	"archive/zip"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// extractDWARF unpacks testdata/ios/ShopApp.dSYM.zip (built from testdata/ios/src, fat arm64+x86_64).
func extractDWARF(t *testing.T) string {
	t.Helper()
	zr, err := zip.OpenReader("../../testdata/ios/ShopApp.dSYM.zip")
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	for _, f := range zr.File {
		if strings.HasSuffix(f.Name, "Contents/Resources/DWARF/ShopApp") {
			rc, _ := f.Open()
			defer rc.Close()
			out := filepath.Join(t.TempDir(), "ShopApp")
			w, _ := os.Create(out)
			io.Copy(w, rc)
			w.Close()
			return out
		}
	}
	t.Fatal("DWARF file not in zip")
	return ""
}

const (
	arm64UUID  = "4089D1C19AD13B2485D2527099F8E383"
	x86_64UUID = "D9AD89F73B143229A1FE606B2315D32C"
)

func TestInspectFat(t *testing.T) {
	ids, err := Inspect(extractDWARF(t))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, id := range ids {
		got[id.Arch] = id.ID
	}
	if got["arm64"] != arm64UUID || got["x86_64"] != x86_64UUID {
		t.Errorf("ids = %+v", ids)
	}
	if _, err := Inspect("../../testdata/android/mapping.txt"); err == nil {
		t.Error("mapping.txt inspected as debug file")
	}
}

func TestSymbolicateAppleCrash(t *testing.T) {
	path := extractDWARF(t)
	sym, err := Open(path, "4089D1C1-9AD1-3B24-85D2-527099F8E383")
	if err != nil {
		t.Fatal(err)
	}
	defer sym.Close()
	if sym.Arch != "arm64" || sym.TextVMAddr != 0x100000000 {
		t.Fatalf("sym = %+v", sym.Ident)
	}
	crash, _ := os.ReadFile("../../testdata/ios/crash.txt")
	for _, tc := range []struct {
		name     string
		demangle func([]string) map[string]string
		closure  string
	}{
		{"no demangler", nil, "$s7ShopApp15CheckoutServiceV8checkoutys5Int32VSayAFGFAFSRyAFGXEfU_"},
		{"stub demangler", func(n []string) map[string]string {
			return map[string]string{"$s7ShopApp15CheckoutServiceV8checkoutys5Int32VSayAFGFAFSRyAFGXEfU_": "closure #1 in CheckoutService.checkout(_:)"}
		}, "closure #1 in CheckoutService.checkout(_:)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := SymbolicateApple(string(crash), func(id string) (*Symbolizer, bool) {
				if id == arm64UUID {
					return sym, true
				}
				return nil, false
			}, tc.demangle)
			if !res.Symbolicated {
				t.Fatal("not symbolicated")
			}
			want := []struct {
				fn, file string
				line     int
			}{
				{"validate_price", "helper.c", 6},
				{"compute_total", "helper.c", 14},
				{tc.closure, "main.swift", 7},
				{"", "main.swift", 6}, // checkout: DW_AT_name differs per demangler; checked below
				{"main", "main.swift", 12},
			}
			fr := res.Frames[3:8]
			for i, w := range want {
				f := fr[i]
				if (w.fn != "" && f.Fn != w.fn) || ShortFile(f.File) != w.file || f.Line != w.line || !f.InApp || !f.Symbolicated {
					t.Errorf("frame %d = %+v, want %+v", i+3, f, w)
				}
			}
			if !strings.Contains(fr[3].Fn, "checkout") || fr[3].Mangled == "" {
				t.Errorf("checkout frame = %+v", fr[3])
			}
			if res.Frames[0].Symbolicated || res.Frames[0].InApp {
				t.Errorf("system frame = %+v", res.Frames[0])
			}
			if len(res.MissingIDs) != 0 {
				t.Errorf("missing = %v", res.MissingIDs)
			}
			if !strings.Contains(res.Stack, "validate_price (in ShopApp) (helper.c:6)") || !strings.Contains(res.Stack, "Binary Images:") {
				t.Errorf("stack:\n%s", res.Stack)
			}
		})
	}
	t.Run("missing dSYM", func(t *testing.T) {
		res := SymbolicateApple(string(crash), func(string) (*Symbolizer, bool) { return nil, false }, nil)
		if res.Symbolicated || len(res.MissingIDs) != 1 || res.MissingIDs[0] != arm64UUID || res.Stack != string(crash) {
			t.Errorf("res = %+v", res)
		}
	})
}

func TestRealDemangler(t *testing.T) {
	if SwiftDemanglerPath() == "" {
		t.Skip("swift-demangle not available")
	}
	m := DemangleSwift([]string{"$s7ShopApp15CheckoutServiceV8checkoutys5Int32VSayAFGF"})
	if got := m["$s7ShopApp15CheckoutServiceV8checkoutys5Int32VSayAFGF"]; got != "CheckoutService.checkout(_:)" {
		t.Errorf("demangled = %q", got)
	}
}

func TestSymbolicateDart(t *testing.T) {
	for _, tc := range []struct {
		name, symbols, trace string
		want                 []string // "fn file:line" of the first frames
	}{
		// real trace from `dart compile exe --save-debugging-info` on macOS (Mach-O companion)
		{"macos mach-o", "shop.symbols", "trace-macos.txt", []string{"PriceValidator.validate main.dart:4", "CartService.computeTotal main.dart:17"}},
		// Android-style trace for the linux/arm64 ELF symbols of the same program
		{"android elf", "shop-linux.symbols", "trace-android.txt", []string{"PriceValidator.validate main.dart:4", "CartService.computeTotal main.dart:17"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := "../../testdata/flutter/" + tc.symbols
			ids, err := Inspect(path)
			if err != nil || len(ids) != 1 {
				t.Fatalf("inspect: %v %v", ids, err)
			}
			trace, _ := os.ReadFile("../../testdata/flutter/" + tc.trace)
			if !IsDartTrace(string(trace)) || DartBuildID(string(trace)) != ids[0].ID {
				t.Fatalf("build id %s vs %s", DartBuildID(string(trace)), ids[0].ID)
			}
			sym, err := Open(path, ids[0].ID)
			if err != nil {
				t.Fatal(err)
			}
			defer sym.Close()
			res := SymbolicateDart(string(trace), func(id string) (*Symbolizer, bool) { return sym, id == ids[0].ID })
			if !res.Symbolicated {
				t.Fatal("not symbolicated")
			}
			for i, w := range tc.want {
				f := res.Frames[i]
				if got := f.Fn + " " + ShortFile(f.File) + ":" + strconv.Itoa(f.Line); got != w {
					t.Errorf("frame %d = %q, want %q (%+v)", i, got, w, f)
				}
			}
			if !strings.Contains(res.Stack, "PriceValidator.validate (") {
				t.Errorf("stack:\n%s", res.Stack)
			}
		})
	}
}

func TestBadInputs(t *testing.T) {
	dir := t.TempDir()
	for name, data := range map[string][]byte{
		"empty":      {},
		"text":       []byte("hello"),
		"fake-macho": append([]byte{0xcf, 0xfa, 0xed, 0xfe, 0x0c, 0, 0, 1}, make([]byte, 64)...),
		"fake-fat":   append([]byte{0xca, 0xfe, 0xba, 0xbe, 0, 0, 0, 9}, make([]byte, 40)...),
		"fake-elf":   append([]byte{0x7f, 'E', 'L', 'F', 2, 1, 1}, make([]byte, 80)...),
	} {
		p := filepath.Join(dir, name)
		os.WriteFile(p, data, 0o600)
		if _, err := Inspect(p); err == nil {
			t.Errorf("Inspect(%s) ok", name)
		}
		if _, err := Open(p, ""); err == nil {
			t.Errorf("Open(%s) ok", name)
		}
	}
	// garbage stacks never panic and are returned unchanged
	for _, s := range []string{"", "Binary Images:\n0xzz - 0x1 x y <q> /p", "*** *** ***\nbuild_id: 'ab'\n    #00 abs zz"} {
		if r := SymbolicateApple(s, func(string) (*Symbolizer, bool) { return nil, false }, nil); r.Stack != s {
			t.Errorf("apple changed %q", s)
		}
		if r := SymbolicateDart(s, func(string) (*Symbolizer, bool) { return nil, false }); r.Stack != s {
			t.Errorf("dart changed %q", s)
		}
	}
}

// testdata/ndk/libshop.so: clang -target aarch64-linux-android24 -g -O1 -shared (lld, --build-id=sha1) of shop.c;
// validate_price is inlined into compute_total.
func TestSymbolicateTombstone(t *testing.T) {
	const so = "../../testdata/ndk/libshop.so"
	ids, err := Inspect(so)
	if err != nil || len(ids) != 1 || ids[0].ID != "1B7D8B602688B7DEFD914A42844E8D4A71A6648A" || ids[0].Format != "elf" {
		t.Fatalf("inspect: %+v %v", ids, err)
	}
	sym, err := Open(so, ids[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	defer sym.Close()
	stack, _ := os.ReadFile("../../testdata/ndk/tombstone.txt")
	if !IsTombstone(string(stack)) || IsDartTrace(string(stack)) || IsAppleReport(string(stack)) {
		t.Fatal("detection")
	}
	res := SymbolicateTombstone(string(stack), func(id string) (*Symbolizer, bool) { return sym, id == ids[0].ID })
	if !res.Symbolicated || len(res.MissingIDs) != 0 {
		t.Fatalf("res = %+v", res)
	}
	want := []struct {
		fn, file string
		line     int
		inlined  bool
		inApp    bool
		sym      bool
	}{
		{"abort", "", 0, false, false, false},
		{"validate_price", "shop.c", 5, true, true, true}, // pc used as-is (SDK already adjusted it)
		{"compute_total", "shop.c", 13, false, true, true},
		{"Java_com_example_shop_Native_total", "shop.c", 19, false, true, true}, // base.apk!libshop.so
		{"art_quick_generic_jni_trampoline", "", 0, false, false, false},
	}
	if len(res.Frames) != len(want) {
		t.Fatalf("frames %d:\n%s", len(res.Frames), res.Stack)
	}
	for i, w := range want {
		f := res.Frames[i]
		if f.Fn != w.fn || ShortFile(f.File) != w.file || (w.line != 0 && f.Line != w.line) || f.Inlined != w.inlined || f.InApp != w.inApp || f.Symbolicated != w.sym {
			t.Errorf("frame %d = %+v, want %+v", i, f, w)
		}
	}
	if !strings.Contains(res.Stack, "#01 pc 00000000000003e0  /data/app/~~a1b2c3==/com.example.shop-d4e5f6==/lib/arm64/libshop.so (validate_price) (shop.c:5) [inlined]") {
		t.Errorf("stack:\n%s", res.Stack)
	}
	// without the library: in-app build id reported missing, system libs not
	res = SymbolicateTombstone(string(stack), func(string) (*Symbolizer, bool) { return nil, false })
	if res.Symbolicated || len(res.MissingIDs) != 1 || res.MissingIDs[0] != ids[0].ID || res.Stack != string(stack) {
		t.Errorf("missing = %+v", res.MissingIDs)
	}
}

func TestAppleAllThreads(t *testing.T) {
	sym, err := Open(extractDWARF(t), arm64UUID)
	if err != nil {
		t.Fatal(err)
	}
	defer sym.Close()
	crash, _ := os.ReadFile("../../testdata/ios/crash-threads.txt")
	res := SymbolicateApple(string(crash), func(id string) (*Symbolizer, bool) { return sym, id == arm64UUID }, nil)
	var got []string
	for _, f := range res.Frames {
		if f.Symbolicated {
			got = append(got, f.Fn+":"+strconv.Itoa(f.Line))
		}
	}
	// thread 0 frame 1 (main), crashed thread frames 3..7, thread 2 nothing
	if len(got) != 6 || got[0] != "main:12" || got[1] != "validate_price:6" || got[5] != "main:12" {
		t.Errorf("symbolicated frames = %v", got)
	}
	for _, h := range []string{"Thread 0:\n", "Thread 1 Crashed:\n", "Thread 2:\n0   libsystem_pthread.dylib"} {
		if !strings.Contains(res.Stack, h) {
			t.Errorf("lost %q", h)
		}
	}
}
