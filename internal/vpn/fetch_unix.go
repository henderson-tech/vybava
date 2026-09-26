//go:build !windows

package vpn

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// Fetch pulls the tunnel's vault profile into memory: Onyx injects it into
// `vpn _apply`, which validates it and writes it into a private FIFO this
// process reads. The profile never touches a regular file on this side.
func Fetch(ctx context.Context, exe, dir, name string, p Profile) (string, error) {
	tmp, err := os.MkdirTemp("", "vybava-vpn-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp)
	fifo := filepath.Join(tmp, name+".conf")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		return "", fmt.Errorf("create the profile pipe: %w", err)
	}
	type read struct {
		b   []byte
		err error
	}
	got := make(chan read, 1)
	go func() {
		f, err := os.Open(fifo) // blocks until the child opens its end
		if err != nil {
			got <- read{nil, err}
			return
		}
		defer f.Close()
		b, err := io.ReadAll(io.LimitReader(f, maxConfig+1))
		got <- read{b, err}
	}()
	invokeErr := Invoke(ctx, ApplyArgv(exe, name, dir, fifo), p.Ref)
	// A child that never opened the pipe leaves the reader blocked in open;
	// an empty writer releases it (retried: the reader may not be there yet).
	// Harmless once the child's data is in — it only adds an EOF.
	var r read
	for waiting := true; waiting; {
		if fd, err := syscall.Open(fifo, syscall.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
			syscall.Close(fd)
		}
		select {
		case r = <-got:
			waiting = false
		case <-time.After(100 * time.Millisecond):
		}
	}
	switch {
	case invokeErr != nil:
		return "", invokeErr
	case r.err != nil:
		return "", fmt.Errorf("read the profile pipe: %w", r.err)
	case len(r.b) == 0:
		return "", errors.New("the Onyx child delivered no profile")
	case len(r.b) > maxConfig:
		return "", errors.New("the vault profile is larger than 16 KiB")
	}
	return string(r.b), nil
}
