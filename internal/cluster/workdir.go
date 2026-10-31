package cluster

import (
	"os"
	"path/filepath"
)

// WorkDir is a private working directory for one run. It holds the run's
// kubeconfig and lives outside the evidence bundle, so credentials are never
// copied into evidence.
type WorkDir struct {
	// Dir is the absolute path of the directory.
	Dir string
	// Kubeconfig is the path the run's kubeconfig will be written to.
	Kubeconfig string
}

// NewWorkDir creates a new directory under base (the system temp directory if
// base is empty). os.MkdirTemp creates it with mode 0700, so the kubeconfig
// written inside is reachable only by the current user.
func NewWorkDir(base string) (*WorkDir, error) {
	dir, err := os.MkdirTemp(base, "deployora-run-")
	if err != nil {
		return nil, err
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		_ = os.RemoveAll(dir)
		return nil, err
	}
	return &WorkDir{Dir: abs, Kubeconfig: filepath.Join(abs, "kubeconfig")}, nil
}

// Remove deletes the directory and everything in it. It is safe to call twice.
func (w *WorkDir) Remove() error {
	return os.RemoveAll(w.Dir)
}
