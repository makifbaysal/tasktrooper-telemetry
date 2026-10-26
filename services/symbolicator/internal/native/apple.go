package native

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/tasktrooper/telemetry/services/symbolicator/internal/frame"
)

// Provider returns the symbolizer for a normalized debug id, or false when no debug file was uploaded.
type Provider func(id string) (*Symbolizer, bool)

// Image is one "Binary Images:" entry of an Apple crash report.
type Image struct {
	Load, End uint64
	Name      string
	Arch      string
	ID        string // normalized
	Path      string
}

var (
	// "3   ShopApp                        0x0000000104a1d490 0x104a1c000 + 5264"
	appleFrameRe = regexp.MustCompile(`^(\d+)\s+(\S.*?)\s+0x([0-9a-fA-F]+)(?:\s+(.*))?$`)
	// "0x104a1c000 - 0x104a1ffff ShopApp arm64 <4089D1C1-...> /path/ShopApp" (also "+ShopApp", "arm64  <uuid>")
	appleImageRe = regexp.MustCompile(`^\s*0x([0-9a-fA-F]+)\s*-\s*0x([0-9a-fA-F]+)\s+\+?(.+?)\s+(\S+)\s+<([0-9a-fA-F-]+)>\s*(.*)$`)
)

// ParseImages extracts the Binary Images section.
func ParseImages(stack string) []Image {
	var out []Image
	for _, l := range strings.Split(stack, "\n") {
		if m := appleImageRe.FindStringSubmatch(l); m != nil {
			lo, _ := strconv.ParseUint(m[1], 16, 64)
			hi, _ := strconv.ParseUint(m[2], 16, 64)
			out = append(out, Image{Load: lo, End: hi, Name: m[3], Arch: m[4], ID: NormalizeID(m[5]), Path: strings.TrimSpace(m[6])})
		}
	}
	return out
}

// IsAppleReport reports whether stack looks like an Apple-format crash report.
func IsAppleReport(stack string) bool {
	return strings.Contains(stack, "Binary Images:")
}

// Result of symbolicating a native stack.
type Result struct {
	Frames       []frame.Frame
	Stack        string
	Symbolicated bool
	MissingIDs   []string // debug ids of in-app images without an uploaded debug file
}

// SymbolicateApple symbolicates an Apple crash report (iOS SDK format, contracts.md §12).
func SymbolicateApple(stack string, provide Provider, demangle func([]string) map[string]string) Result {
	images := ParseImages(stack)
	type pending struct {
		lineIdx int
		frames  []frame.Frame
		idx     string
	}
	lines := strings.Split(strings.ReplaceAll(stack, "\r\n", "\n"), "\n")
	var res Result
	var items []pending
	missing := map[string]bool{}
	inImages := false
	for li, l := range lines {
		if strings.HasPrefix(strings.TrimSpace(l), "Binary Images:") {
			inImages = true
		}
		if inImages {
			continue
		}
		m := appleFrameRe.FindStringSubmatch(strings.TrimSpace(l))
		if m == nil {
			continue
		}
		idx, _ := strconv.Atoi(m[1])
		addr, err := strconv.ParseUint(m[3], 16, 64)
		if err != nil {
			continue
		}
		name := strings.TrimSpace(m[2])
		img := findImage(images, addr, name)
		base := frame.Frame{Fn: strings.TrimSpace(m[4]), Addr: fmt.Sprintf("0x%016x", addr), Module: name, Raw: l}
		if img != nil {
			base.Module = img.Name
			base.InApp = isAppImage(img.Path)
		}
		p := pending{lineIdx: li, idx: m[1], frames: []frame.Frame{base}}
		if img != nil && img.ID != "" {
			if sym, ok := provide(img.ID); ok && sym != nil {
				lookup := addr - img.Load + sym.TextVMAddr
				if idx > 0 && lookup > 0 {
					lookup-- // return address → call instruction
				}
				if locs, ok := sym.Lookup(lookup); ok {
					p.frames = p.frames[:0]
					for _, loc := range locs {
						f := base
						f.Fn, f.File, f.Line, f.Col, f.Inlined, f.Symbolicated = loc.Fn, loc.File, loc.Line, loc.Col, loc.Inlined, true
						if loc.Linkage != "" && loc.Linkage != loc.Fn {
							f.Mangled = loc.Linkage
						}
						if IsSwiftMangled(loc.Linkage) {
							f.Mangled = loc.Linkage
						}
						p.frames = append(p.frames, f)
					}
					res.Symbolicated = true
				}
			} else if base.InApp {
				missing[img.ID] = true
			}
		}
		items = append(items, p)
	}
	// demangle Swift names in one batch
	var names []string
	for _, p := range items {
		for _, f := range p.frames {
			if IsSwiftMangled(f.Mangled) {
				names = append(names, f.Mangled)
			}
		}
	}
	var dm map[string]string
	if len(names) > 0 && demangle != nil {
		dm = demangle(names)
	}
	for pi := range items {
		var rendered []string
		for fi := range items[pi].frames {
			f := &items[pi].frames[fi]
			if d, ok := dm[f.Mangled]; ok && d != "" {
				f.Fn = d
			}
			res.Frames = append(res.Frames, *f)
			if f.Symbolicated {
				loc := ""
				if f.File != "" {
					loc = fmt.Sprintf(" (%s:%d)", ShortFile(f.File), f.Line)
				}
				inl := ""
				if f.Inlined {
					inl = " [inlined]"
				}
				rendered = append(rendered, fmt.Sprintf("%-4s%-30s %s %s (in %s)%s%s", items[pi].idx, f.Module, f.Addr, f.Fn, f.Module, loc, inl))
			}
		}
		if len(rendered) > 0 {
			lines[items[pi].lineIdx] = strings.Join(rendered, "\n")
		}
	}
	for id := range missing {
		res.MissingIDs = append(res.MissingIDs, id)
	}
	res.Stack = strings.Join(lines, "\n")
	return res
}

func findImage(images []Image, addr uint64, name string) *Image {
	for i := range images {
		if addr >= images[i].Load && addr <= images[i].End {
			return &images[i]
		}
	}
	for i := range images {
		if images[i].Name == name {
			return &images[i]
		}
	}
	return nil
}

func isAppImage(path string) bool {
	if path == "" {
		return false
	}
	for _, p := range []string{"/usr/lib/", "/System/", "/Developer/", "/usr/libexec/", "/Library/Apple/"} {
		if strings.HasPrefix(path, p) {
			return false
		}
	}
	return true
}

// IsSwiftMangled reports whether s is a Swift mangled symbol.
func IsSwiftMangled(s string) bool {
	for _, p := range []string{"$s", "_$s", "$S", "_$S", "$e", "_$e", "_T0"} {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}
