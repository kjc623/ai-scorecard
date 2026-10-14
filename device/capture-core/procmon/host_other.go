//go:build !windows

package procmon

// fileVersion reads no version resource here.
func fileVersion(string) string { return "" }
