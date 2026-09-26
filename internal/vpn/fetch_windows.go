package vpn

import (
	"context"
	"errors"
	"os"
)

func openPipe(string) (*os.File, error) {
	return nil, errors.New("tunnels are managed on macOS only")
}

// Fetch needs a FIFO; tunnels are managed on macOS only.
func Fetch(context.Context, string, string, string, Profile) (string, error) {
	return "", errors.New("tunnels are managed on macOS only")
}
