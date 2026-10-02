package journeys

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// PrivatePath resolves existing ancestors before checking containment, including
// a new plan or evidence directory whose final components do not exist yet.
func PrivatePath(root, target string) (string, error) {
	if target == "" {
		return "", problem("TARGET_UNSAFE", "a private path outside the repository is required")
	}
	r, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	r, err = filepath.EvalSymlinks(r)
	if err != nil {
		return "", err
	}
	p, err := filepath.Abs(target)
	if err != nil {
		return "", err
	}
	ancestor, suffix := p, []string{}
	for {
		_, err = os.Lstat(ancestor)
		if err == nil {
			break
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		suffix = append(suffix, filepath.Base(ancestor))
		ancestor = filepath.Dir(ancestor)
	}
	ancestor, err = filepath.EvalSymlinks(ancestor)
	if err != nil {
		return "", err
	}
	for i := len(suffix) - 1; i >= 0; i-- {
		ancestor = filepath.Join(ancestor, suffix[i])
	}
	rel, err := filepath.Rel(r, ancestor)
	if err != nil {
		return "", err
	}
	if rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", problem("TARGET_UNSAFE", "private plans and evidence must live outside the repository")
	}
	return ancestor, nil
}
