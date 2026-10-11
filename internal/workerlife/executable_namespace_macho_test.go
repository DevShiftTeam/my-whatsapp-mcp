//go:build darwin || linux

package workerlife

import (
	"bytes"
	"context"
	"encoding/binary"
	"testing"
)

func TestExecutableMachOAuditsEveryDylibVariant(t *testing.T) {
	for _, command := range []uint32{0xc, 0x80000018, 0x8000001f, 0x20, 0x80000023} {
		for _, name := range []string{"/usr/lib/libobjc.A.dylib", "/System/Library/Frameworks/Foundation.framework/Versions/C/Foundation", "relative.dylib", "@loader_path/x.dylib", "@rpath/x.dylib", "/usr/lib/../../tmp/x.dylib", "/tmp/x.dylib"} {
			data := namespaceTestMachO(namespaceTestLoad(command, name))
			err := auditExecutableMachO(context.Background(), bytes.NewReader(data), int64(len(data)))
			want := name == "/usr/lib/libobjc.A.dylib" || name == "/System/Library/Frameworks/Foundation.framework/Versions/C/Foundation"
			if (err == nil) != want {
				t.Fatalf("command=%#x name=%q error=%v", command, name, err)
			}
		}
	}
}

func TestExecutableMachORejectsDyldEnvironmentRpathAndUnknownCommands(t *testing.T) {
	for _, command := range []uint32{0x8000001c, 0x27, 0xd, 0xf, 0x10, 0x12, 0x13, 0x14, 0x15, 0x80000077} {
		data := namespaceTestMachO(namespaceTestLoad(command, "/usr/lib/libSystem.B.dylib"))
		if err := auditExecutableMachO(context.Background(), bytes.NewReader(data), int64(len(data))); err == nil {
			t.Fatalf("command %#x accepted", command)
		}
	}
}

func TestExecutableMachORejectsFatWrongCPUWrongTypeAndExtent(t *testing.T) {
	for _, mutate := range []func([]byte){
		func(data []byte) { binary.LittleEndian.PutUint32(data, 0xcafebabe) },
		func(data []byte) { binary.LittleEndian.PutUint32(data[4:], 0x01000007) },
		func(data []byte) { binary.LittleEndian.PutUint32(data[12:], 6) },
		func(data []byte) { binary.LittleEndian.PutUint32(data[16:], 4097) },
		func(data []byte) { binary.LittleEndian.PutUint32(data[20:], uint32(len(data))) },
		func(data []byte) { binary.LittleEndian.PutUint32(data[36:], 4) },
	} {
		data := namespaceTestMachO()
		mutate(data)
		if err := auditExecutableMachO(context.Background(), bytes.NewReader(data), int64(len(data))); err == nil {
			t.Fatal("invalid Mach-O accepted")
		}
	}
}
