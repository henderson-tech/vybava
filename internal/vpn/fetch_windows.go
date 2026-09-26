package vpn

import (
	"context"
	"errors"
)

// Fetch needs a FIFO; tunnels are managed on macOS only.
func Fetch(context.Context, string, string, string, Profile) (string, error) {
	return "", errors.New("tunnels are managed on macOS only")
}
