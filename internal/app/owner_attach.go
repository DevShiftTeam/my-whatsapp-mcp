package app

import "github.com/Bodokh/my-whatsapp-mcp/internal/appdata"

// OwnerAttachURL reads a private candidate from the same config-owned real/demo
// root. It is not proof of a live owner: the caller must authenticate Attach.
func OwnerAttachURL(cfg Config) (string, error) {
	root, err := resolveDataRoot(cfg)
	if err != nil {
		return "", err
	}
	paths, err := appdata.ExistingPaths(root)
	if err != nil {
		return "", err
	}
	return appdata.ReadOwnerURL(paths)
}
