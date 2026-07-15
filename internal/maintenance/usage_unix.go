//go:build !windows

package maintenance

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// Check returns total and used bytes for the filesystem hosting dbPath.
func (RealUsageChecker) Check(dbPath string) (DiskUsage, error) {
	target, err := resolveUsagePath(dbPath)
	if err != nil {
		return DiskUsage{}, err
	}

	var stat unix.Statfs_t
	if err := unix.Statfs(target, &stat); err != nil {
		return DiskUsage{}, fmt.Errorf("statfs %s: %w", target, err)
	}

	total := stat.Blocks * uint64(stat.Bsize)
	available := stat.Bavail * uint64(stat.Bsize)
	used := total - available

	return DiskUsage{
		Path:       target,
		UsedBytes:  used,
		TotalBytes: total,
	}, nil
}
