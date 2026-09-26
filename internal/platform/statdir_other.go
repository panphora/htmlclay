//go:build !windows && !darwin

package platform

// statDir follows symlinks deliberately: a stored trusted-folder path whose
// directory was later swapped for a symlink stats through to the link's target,
// whose fingerprint differs from the recorded one, which is exactly the swap
// the comparison exists to catch. No stable volume id is read here yet, so
// fingerprints stay dev:inode.
func statDir(path string) (dirStat, bool) {
	return statPath(path)
}
