package dylib

import (
	"bytes"
	"debug/elf"
	"debug/macho"
	"debug/pe"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
)

// ExportedFunctions lists the FerroGo ABI functions (ferrogo_v1_call_<name>)
// defined by a shared library, by reading its symbol table without loading
// or executing it.
func ExportedFunctions(path string) ([]string, error) {
	symbols, err := exportedSymbols(path)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var names []string
	for _, s := range symbols {
		s = strings.TrimPrefix(s, "_") // Mach-O symbol prefix
		if fn, ok := strings.CutPrefix(s, callPrefix); ok && !seen[fn] {
			seen[fn] = true
			names = append(names, fn)
		}
	}
	sort.Strings(names)
	return names, nil
}

func exportedSymbols(path string) ([]string, error) {
	magic := make([]byte, 4)
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	_, err = f.ReadAt(magic, 0)
	f.Close()
	if err != nil {
		return nil, err
	}
	switch {
	case bytes.Equal(magic, []byte("\x7fELF")):
		return elfSymbols(path)
	case magic[0] == 'M' && magic[1] == 'Z':
		return peSymbols(path)
	default:
		return machoSymbols(path)
	}
}

func elfSymbols(path string) ([]string, error) {
	f, err := elf.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	syms, err := f.DynamicSymbols()
	if err != nil {
		return nil, err
	}
	var out []string
	for _, s := range syms {
		if s.Section != elf.SHN_UNDEF && elf.ST_TYPE(s.Info) == elf.STT_FUNC {
			out = append(out, s.Name)
		}
	}
	return out, nil
}

func machoSymbols(path string) ([]string, error) {
	f, err := macho.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if f.Symtab == nil {
		return nil, nil
	}
	const nExt = 0x01
	var out []string
	for _, s := range f.Symtab.Syms {
		if s.Sect != 0 && s.Type&nExt != 0 {
			out = append(out, s.Name)
		}
	}
	return out, nil
}

// peSymbols reads the names in a PE export directory (debug/pe does not
// expose exports).
func peSymbols(path string) ([]string, error) {
	f, err := pe.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var dir pe.DataDirectory
	switch oh := f.OptionalHeader.(type) {
	case *pe.OptionalHeader64:
		if oh.NumberOfRvaAndSizes > pe.IMAGE_DIRECTORY_ENTRY_EXPORT {
			dir = oh.DataDirectory[pe.IMAGE_DIRECTORY_ENTRY_EXPORT]
		}
	case *pe.OptionalHeader32:
		if oh.NumberOfRvaAndSizes > pe.IMAGE_DIRECTORY_ENTRY_EXPORT {
			dir = oh.DataDirectory[pe.IMAGE_DIRECTORY_ENTRY_EXPORT]
		}
	}
	if dir.VirtualAddress == 0 {
		return nil, nil
	}
	read := func(rva, n uint32) ([]byte, error) {
		for _, s := range f.Sections {
			if rva >= s.VirtualAddress && rva-s.VirtualAddress < max(s.VirtualSize, s.Size) {
				data, err := s.Data()
				if err != nil {
					return nil, err
				}
				off := rva - s.VirtualAddress
				if uint64(off)+uint64(n) > uint64(len(data)) {
					return nil, errors.New("export data out of range")
				}
				return data[off : off+n], nil
			}
		}
		return nil, fmt.Errorf("no section contains RVA %#x", rva)
	}
	hdr, err := read(dir.VirtualAddress, 40)
	if err != nil {
		return nil, err
	}
	numNames := binary.LittleEndian.Uint32(hdr[24:])
	namesRVA := binary.LittleEndian.Uint32(hdr[32:])
	if numNames > 1<<20 {
		return nil, errors.New("implausible export count")
	}
	table, err := read(namesRVA, 4*numNames)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, numNames)
	for i := range numNames {
		nameRVA := binary.LittleEndian.Uint32(table[4*i:])
		var name []byte
		for j := uint32(0); j < 1024; j++ {
			b, err := read(nameRVA+j, 1)
			if err != nil {
				return nil, err
			}
			if b[0] == 0 {
				break
			}
			name = append(name, b[0])
		}
		out = append(out, string(name))
	}
	return out, nil
}
