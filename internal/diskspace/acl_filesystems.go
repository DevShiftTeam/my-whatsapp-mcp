package diskspace

// Linux filesystem magic values with established local POSIX access ACL/mode
// semantics. Refuse NFS, CIFS/SMB, FUSE and unknown ACL models. Adding support
// requires verifying that named grants cannot bypass the 0700 mode mask.
func posixMaskFilesystem(kind uint64) bool {
	switch kind {
	case 0xef53, // ext2, ext3, ext4
		0x9123683e, // btrfs
		0x58465342, // xfs
		0x1021994,  // tmpfs
		0x858458f6, // ramfs
		0x794c7630, // overlayfs
		0xf2f52010: // f2fs
		return true
	default:
		return false
	}
}
