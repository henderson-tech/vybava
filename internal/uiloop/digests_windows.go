//go:build windows

package uiloop

import "os"

// statStamp stamps nothing on Windows, which has no inode or ctime to prove a
// file unchanged, so every read hashes there.
func statStamp(file string) (fileStamp, bool, error) {
	_, err := os.Stat(file)
	return fileStamp{}, false, err
}
