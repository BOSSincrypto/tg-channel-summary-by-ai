package maintenance

import (
	"fmt"
	"os"
	"path/filepath"
)

// DiskUsage describes the database volume usage for the filesystem hosting the DB.
type DiskUsage struct {
	Path       string
	UsedBytes  uint64
	TotalBytes uint64
}

// UsedPercent returns the used capacity percentage for the backing volume.
func (d DiskUsage) UsedPercent() float64 {
	if d.TotalBytes == 0 {
		return 0
	}
	return float64(d.UsedBytes) * 100 / float64(d.TotalBytes)
}

// UsageChecker inspects the backing volume for the database path.
type UsageChecker interface {
	Check(dbPath string) (DiskUsage, error)
}

// RealUsageChecker inspects the local filesystem containing the database.
type RealUsageChecker struct{}

func resolveUsagePath(dbPath string) (string, error) {
	if dbPath == "" {
		return "", fmt.Errorf("database path is required")
	}

	absPath, err := filepath.Abs(dbPath)
	if err != nil {
		return "", fmt.Errorf("resolve database path: %w", err)
	}

	info, err := os.Stat(absPath)
	switch {
	case err == nil && info.IsDir():
		return absPath, nil
	case err == nil:
		return filepath.Dir(absPath), nil
	case os.IsNotExist(err):
		return filepath.Dir(absPath), nil
	default:
		return "", fmt.Errorf("stat database path: %w", err)
	}
}
