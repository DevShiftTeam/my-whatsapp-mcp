package diskspace

import "testing"

func TestFilesystemUnitsUseAllocationFragments(t *testing.T) {
	// Linux available counts in fragments must not be inflated by the I/O size.
	for _, test := range []struct {
		name                    string
		blockSize, fragmentSize int64
		wantUnit                uint64
	}{
		{"different_fragment_size", 65536, 4096, 4096},
		{"missing_fragment_falls_back", 4096, 0, 4096},
		{"larger_fragment", 4096, 65536, 65536},
	} {
		t.Run(test.name, func(t *testing.T) {
			sample, err := spaceFromUnits(4, test.blockSize, test.fragmentSize)
			if err != nil || sample.AvailableBlocks != 4 || sample.BlockSize != test.wantUnit {
				t.Fatalf("wrong allocation unit: %+v %v", sample, err)
			}
		})
	}
	for _, values := range [][2]int64{{0, 0}, {-1, 0}, {4096, -1}, {0, 4096}} {
		if _, err := spaceFromUnits(4, values[0], values[1]); err != ErrUnavailable {
			t.Fatalf("invalid block/fragment sizes %v: %v", values, err)
		}
	}
}

func TestPOSIXMaskFilesystemCheckRejectsRichOrUnknownACLs(t *testing.T) {
	// A 0700 mask cannot authorize Linux NFSv4/SMB named ACLs independently.
	for _, kind := range []uint64{0x6969, 0xff534d42, 0xfe534d42, 0x65735546, 0, 0xdeadbeef} {
		if posixMaskFilesystem(kind) {
			t.Fatalf("unverified ACL filesystem accepted: %x", kind)
		}
	}
	for _, kind := range []uint64{0xef53, 0x9123683e, 0x58465342, 0x1021994, 0x858458f6, 0x794c7630, 0xf2f52010} {
		if !posixMaskFilesystem(kind) {
			t.Fatalf("known POSIX filesystem rejected: %x", kind)
		}
	}
}
