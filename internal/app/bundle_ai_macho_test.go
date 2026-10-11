package app

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func machoDependencyFixture(dependency string) []byte {
	length := (24 + len(dependency) + 1 + 7) &^ 7
	data := make([]byte, 32+length+32)
	for offset, value := range map[int]uint32{0: 0xfeedfacf, 4: 0x0100000c, 12: 2, 16: 2, 20: uint32(length + 32), 32: 0x0c, 36: uint32(length), 40: 24} {
		binary.LittleEndian.PutUint32(data[offset:offset+4], value)
	}
	copy(data[56:], dependency)
	dyld := data[32+length:]
	binary.LittleEndian.PutUint32(dyld[:4], 0xe)
	binary.LittleEndian.PutUint32(dyld[4:8], 32)
	binary.LittleEndian.PutUint32(dyld[8:12], 12)
	copy(dyld[12:], "/usr/lib/dyld")
	return data
}

func TestMachORejectsUnqualifiedLoaderCommands(t *testing.T) {
	for _, command := range []uint32{0x27, 0xf, 0x10, 0x12, 0x13, 0x14, 0x15, 0x80000077} {
		data := machoDependencyFixture("/usr/lib/libSystem.B.dylib")
		binary.LittleEndian.PutUint32(data[32:36], command)
		if _, err := boundedMachOLibraries(bytes.NewReader(data), int64(len(data)), true); err == nil {
			t.Fatalf("unsupported loader command %x qualified", command)
		}
	}
	data := machoDependencyFixture("/usr/lib/libSystem.B.dylib")
	copy(data[len(data)-20:], "/tmp/evil-dyld")
	if _, err := boundedMachOLibraries(bytes.NewReader(data), int64(len(data)), true); err == nil {
		t.Fatal("foreign interpreter qualified")
	}
}

func TestBoundedMachOAuditRejectsAdvertisedAllocationAndAmbientLoader(t *testing.T) {
	data := machoDependencyFixture("/usr/lib/libSystem.B.dylib")
	dependencies, err := boundedMachOLibraries(bytes.NewReader(data), int64(len(data)), true)
	if err != nil || len(dependencies) != 1 || dependencies[0] != "/usr/lib/libSystem.B.dylib" {
		t.Fatal("valid bounded header refused")
	}
	for _, offset := range []int{16, 20, 36, 40} {
		changed := append([]byte(nil), data...)
		binary.LittleEndian.PutUint32(changed[offset:offset+4], 0xffffffff)
		if _, err := boundedMachOLibraries(bytes.NewReader(changed), int64(len(changed)), true); err == nil {
			t.Fatalf("unbounded header accepted at %d", offset)
		}
	}
	binary.LittleEndian.PutUint32(data[32:36], 0x8000001c)
	if _, err := boundedMachOLibraries(bytes.NewReader(data), int64(len(data)), true); err == nil {
		t.Fatal("ambient rpath accepted")
	}
}
