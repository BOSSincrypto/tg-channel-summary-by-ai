//go:build windows

package maintenance

import (
	"fmt"
	"path/filepath"

	"golang.org/x/sys/windows"
)

// Check returns total and used bytes for the filesystem hosting dbPath.
func (RealUsageChecker) Check(dbPath string) (DiskUsage, error) {
	target, err := resolveUsagePath(dbPath)
	if err != nil {
		return DiskUsage{}, err
	}

	volumeRoot := filepath.VolumeName(target) + `\`
	if volumeRoot == `\` {
		return DiskUsage{}, fmt.Errorf("determine volume root for %s", target)
	}

	pathPtr, err := windows.UTF16PtrFromString(volumeRoot)
	if err != nil {
		return DiskUsage{}, fmt.Errorf("encode volume root %s: %w", volumeRoot, err)
	}

	var freeBytesAvailable, totalNumberOfBytes, totalNumberOfFreeBytes uint64
	if err := windows.GetDiskFreeSpaceEx(pathPtr, &freeBytesAvailable, &totalNumberOfBytes, &totalNumberOfFreeBytes); err != nil {
		return DiskUsage{}, fmt.Errorf("GetDiskFreeSpaceEx %s: %w", volumeRoot, err)
	}

	total := totalNumberOfBytes
	free := totalNumberOfFreeBytes
	used := total - free

	return DiskUsage{
		Path:       target,
		UsedBytes:  used,
		TotalBytes: total,
	}, nil
}
