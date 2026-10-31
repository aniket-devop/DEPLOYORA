package cluster

import (
	"fmt"
	"strings"
)

// NamePrefix starts the name of every cluster Deployora creates. Delete and List
// refuse any other name, so a user's own clusters are never touched.
const NamePrefix = "deployora-"

// maxNameLen is a conservative cap that keeps container names such as
// "<name>-control-plane" within hostname limits.
const maxNameLen = 40

// Name returns the cluster name for a run ID, or an error if the result would
// not pass CheckName.
func Name(runID string) (string, error) {
	name := NamePrefix + runID
	if err := CheckName(name); err != nil {
		return "", err
	}
	return name, nil
}

// CheckName reports whether name is a cluster Deployora may create or touch:
// the Deployora prefix, then only lowercase letters, digits and hyphens, not
// ending with a hyphen, and not longer than the cap.
func CheckName(name string) error {
	if !strings.HasPrefix(name, NamePrefix) {
		return fmt.Errorf("%w: cluster name %q must start with %q", ErrInvalid, name, NamePrefix)
	}
	rest := name[len(NamePrefix):]
	if rest == "" {
		return fmt.Errorf("%w: cluster name %q has nothing after the prefix", ErrInvalid, name)
	}
	if len(name) > maxNameLen {
		return fmt.Errorf("%w: cluster name %q is longer than %d characters", ErrInvalid, name, maxNameLen)
	}
	if strings.HasSuffix(name, "-") {
		return fmt.Errorf("%w: cluster name %q must not end with a hyphen", ErrInvalid, name)
	}
	for _, r := range rest {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-') {
			return fmt.Errorf("%w: cluster name %q may contain only a-z, 0-9 and hyphens", ErrInvalid, name)
		}
	}
	return nil
}
