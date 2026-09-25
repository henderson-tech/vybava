//go:build darwin

package reclaim

import "syscall"

// Background moves this whole process into the Darwin background band:
// throttled CPU and disk I/O, what `taskpolicy -b` does to a command, so a
// walk or delete over hundreds of thousands of files yields to interactive
// work. Constants from <sys/resource.h>.
func Background() error {
	const prioDarwinProcess, prioDarwinBG = 4, 0x1000
	return syscall.Setpriority(prioDarwinProcess, 0, prioDarwinBG)
}
