package appdata

import "os"

// WorkerGuard duplicates the existing held lock's open file description. Pass
// the owned descriptor explicitly to a worker and close it after that worker has
// stopped. Each duplicate keeps the flock held even after this lease closes;
// callers must close guards, never explicitly unlock them. Close-on-exec is set
// until a process launcher deliberately inherits the descriptor.
func (lease *InstanceLock) WorkerGuard() (*os.File, error) {
	if lease == nil {
		return nil, ErrLockRequired
	}
	lease.mu.Lock()
	defer lease.mu.Unlock()
	return lease.workerGuardLocked()
}

// The caller must hold lease.mu.
func (lease *InstanceLock) workerGuardLocked() (*os.File, error) {
	if lease.closed || lease.file == nil || lease.root == nil {
		return nil, ErrLockRequired
	}
	canonical, err := canonicalRoot(lease.paths.Root)
	if err != nil {
		return nil, err
	}
	if lease.paths != pathsFor(canonical) {
		return nil, ErrUnsafeDataPath
	}
	dir, err := openNoSymlinkRoot(canonical, false)
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	// The live lease owns its initial tree validation. Workers may now retain
	// verified 0500 executable copies in working, so this duplication validates
	// only the root, ownership marker and same held lock authority.
	if err := verifyOwnership(dir); err != nil {
		return nil, err
	}
	currentRoot, currentErr := dir.Stat(".")
	heldRoot, heldErr := lease.root.Stat(".")
	if currentErr != nil || heldErr != nil || !os.SameFile(currentRoot, heldRoot) {
		return nil, ErrUnsafeDataPath
	}
	guard, err := duplicateLockFile(lease.file)
	if err != nil {
		return nil, err
	}
	current, currentErr := dir.Lstat(instanceLockFile)
	duplicated, duplicateErr := guard.Stat()
	if currentErr != nil || duplicateErr != nil || !privateFile(current) || !privateFile(duplicated) || !os.SameFile(current, duplicated) || !noExtendedACL(guard) {
		guard.Close()
		return nil, ErrUnsafeDataPath
	}
	return guard, nil
}
