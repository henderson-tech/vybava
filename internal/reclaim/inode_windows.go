//go:build windows

package reclaim

import "os"

// hardlinkID has no inode to report on Windows; every file counts.
func hardlinkID(os.FileInfo) (fileID, bool) { return fileID{}, false }
