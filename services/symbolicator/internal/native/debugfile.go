// Package native symbolicates native addresses with DWARF from dSYM bundles (Mach-O, incl. fat/universal
// files) and ELF debug files (Flutter --split-debug-info symbols).
package native

import (
	"debug/dwarf"
	"debug/elf"
	"debug/macho"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"sync"
)

// Ident identifies one debug image inside a file (one per arch slice of a fat Mach-O).
type Ident struct {
	ID     string `json:"id"` // NormalizeID form
	Arch   string `json:"arch"`
	Format string `json:"format"` // macho | elf
}

// NormalizeID turns a Mach-O UUID ("E89216A6-2BB6-...") or ELF/Dart build id ("ce8da3bf...") into
// uppercase hex without dashes, the key used in indexes.
func NormalizeID(s string) string {
	s = strings.TrimSpace(strings.Trim(s, "<>'\""))
	return strings.ToUpper(strings.ReplaceAll(s, "-", ""))
}

// ErrNotDebugFile is returned for files that are neither Mach-O nor ELF.
var ErrNotDebugFile = errors.New("not a Mach-O or ELF file")

// Inspect lists the debug images in path without loading DWARF.
func Inspect(path string) (ids []Ident, err error) {
	defer func() {
		if r := recover(); r != nil {
			ids, err = nil, fmt.Errorf("inspect: malformed file: %v", r)
		}
	}()
	if ff, err := macho.OpenFat(path); err == nil {
		defer ff.Close()
		for _, a := range ff.Arches {
			if id := machoUUID(a.File); id != "" {
				ids = append(ids, Ident{ID: id, Arch: cpuName(a.Cpu, a.SubCpu), Format: "macho"})
			}
		}
		return nonEmpty(ids)
	}
	if f, err := macho.Open(path); err == nil {
		defer f.Close()
		if id := machoUUID(f); id != "" {
			ids = append(ids, Ident{ID: id, Arch: cpuName(f.Cpu, f.SubCpu), Format: "macho"})
		}
		return nonEmpty(ids)
	}
	if f, err := elf.Open(path); err == nil {
		defer f.Close()
		if id := elfBuildID(f); id != "" {
			ids = append(ids, Ident{ID: id, Arch: strings.ToLower(strings.TrimPrefix(f.Machine.String(), "EM_")), Format: "elf"})
		}
		return nonEmpty(ids)
	}
	return nil, ErrNotDebugFile
}

func nonEmpty(ids []Ident) ([]Ident, error) {
	if len(ids) == 0 {
		return nil, errors.New("debug file has no UUID / build id")
	}
	return ids, nil
}

func machoUUID(f *macho.File) string {
	for _, l := range f.Loads {
		raw := l.Raw()
		if len(raw) >= 24 && f.ByteOrder.Uint32(raw[0:4]) == 0x1b { // LC_UUID
			return strings.ToUpper(hex.EncodeToString(raw[8:24]))
		}
	}
	return ""
}

func elfBuildID(f *elf.File) string {
	read := func(data []byte) string {
		bo := f.ByteOrder
		for len(data) >= 12 {
			nsz, dsz, typ := bo.Uint32(data[0:4]), bo.Uint32(data[4:8]), bo.Uint32(data[8:12])
			off := 12 + align4(nsz)
			end := off + align4(dsz)
			if uint64(off)+uint64(dsz) > uint64(len(data)) {
				return ""
			}
			name := strings.TrimRight(string(data[12:12+nsz]), "\x00")
			if typ == 3 && name == "GNU" { // NT_GNU_BUILD_ID
				return strings.ToUpper(hex.EncodeToString(data[off : off+dsz]))
			}
			if int(end) > len(data) {
				return ""
			}
			data = data[end:]
		}
		return ""
	}
	for _, s := range f.Sections {
		if s.Type == elf.SHT_NOTE {
			if data, err := s.Data(); err == nil {
				if id := read(data); id != "" {
					return id
				}
			}
		}
	}
	return ""
}

func align4(n uint32) uint32 { return (n + 3) &^ 3 }

func cpuName(c macho.Cpu, sub uint32) string {
	switch c {
	case macho.CpuArm64:
		if sub&0xff == 2 {
			return "arm64e"
		}
		return "arm64"
	case macho.CpuAmd64:
		return "x86_64"
	case macho.CpuArm:
		return "armv7"
	case macho.Cpu386:
		return "i386"
	}
	return strings.ToLower(c.String())
}

// Symbolizer resolves file-relative addresses of one debug image. Safe for concurrent use.
type Symbolizer struct {
	Ident
	TextVMAddr uint64 // Mach-O __TEXT vmaddr (frame file address = addr - load + TextVMAddr); 0 for ELF

	mu     sync.Mutex
	closer io.Closer
	d      *dwarf.Data
	cus    []cuRange // sorted by lo
	syms   []symbol  // sorted by addr, fallback when DWARF has no match
	built  bool
}

type cuRange struct {
	lo, hi uint64
	off    dwarf.Offset
}

type symbol struct {
	addr uint64
	name string
}

// Open loads the image with the given id from path (any arch slice when id is empty).
func Open(path, id string) (s *Symbolizer, err error) {
	defer func() {
		if r := recover(); r != nil {
			s, err = nil, fmt.Errorf("open: malformed file: %v", r)
		}
	}()
	id = NormalizeID(id)
	if ff, err := macho.OpenFat(path); err == nil {
		for _, a := range ff.Arches {
			if aid := machoUUID(a.File); id == "" || aid == id {
				s, err := fromMachO(a.File, ff)
				if err != nil {
					ff.Close()
				}
				return s, err
			}
		}
		ff.Close()
		return nil, fmt.Errorf("image %s not in fat file", id)
	}
	if f, err := macho.Open(path); err == nil {
		if aid := machoUUID(f); id != "" && aid != id {
			f.Close()
			return nil, fmt.Errorf("image %s not in file (has %s)", id, aid)
		}
		s, err := fromMachO(f, f)
		if err != nil {
			f.Close()
		}
		return s, err
	}
	if f, err := elf.Open(path); err == nil {
		if aid := elfBuildID(f); id != "" && aid != id {
			f.Close()
			return nil, fmt.Errorf("image %s not in file (has %s)", id, aid)
		}
		s, err := fromELF(f)
		if err != nil {
			f.Close()
		}
		return s, err
	}
	return nil, ErrNotDebugFile
}

func fromMachO(f *macho.File, c io.Closer) (*Symbolizer, error) {
	s := &Symbolizer{Ident: Ident{ID: machoUUID(f), Arch: cpuName(f.Cpu, f.SubCpu), Format: "macho"}, closer: c}
	if seg := f.Segment("__TEXT"); seg != nil {
		s.TextVMAddr = seg.Addr
	}
	d, err := f.DWARF()
	if err == nil {
		s.d = d
	}
	if f.Symtab != nil {
		for _, sym := range f.Symtab.Syms {
			if sym.Type&0x0e == 0x0e && sym.Sect > 0 && sym.Type&0xe0 == 0 { // N_SECT, not a stab
				s.syms = append(s.syms, symbol{addr: sym.Value, name: strings.TrimPrefix(sym.Name, "_")})
			}
		}
	}
	if s.d == nil && len(s.syms) == 0 {
		return nil, errors.New("no DWARF and no symbols")
	}
	sort.Slice(s.syms, func(i, j int) bool { return s.syms[i].addr < s.syms[j].addr })
	return s, nil
}

func fromELF(f *elf.File) (*Symbolizer, error) {
	s := &Symbolizer{Ident: Ident{ID: elfBuildID(f), Arch: strings.ToLower(strings.TrimPrefix(f.Machine.String(), "EM_")), Format: "elf"}, closer: f}
	if d, err := f.DWARF(); err == nil {
		s.d = d
	}
	if syms, err := f.Symbols(); err == nil {
		for _, sym := range syms {
			if elf.ST_TYPE(sym.Info) == elf.STT_FUNC || elf.ST_TYPE(sym.Info) == elf.STT_OBJECT && strings.Contains(sym.Name, "Instructions") {
				s.syms = append(s.syms, symbol{addr: sym.Value, name: sym.Name})
			}
		}
	}
	if s.d == nil && len(s.syms) == 0 {
		return nil, errors.New("no DWARF and no symbols")
	}
	sort.Slice(s.syms, func(i, j int) bool { return s.syms[i].addr < s.syms[j].addr })
	return s, nil
}

// Close releases the underlying file.
func (s *Symbolizer) Close() error {
	if s == nil || s.closer == nil {
		return nil
	}
	return s.closer.Close()
}

// OpenFile is Open for a path that must exist; convenience for tests.
func OpenFile(path string) (*Symbolizer, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, err
	}
	return Open(path, "")
}
