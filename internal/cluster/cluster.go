// Package cluster creates and deletes the throwaway clusters a run uses. It only
// ever touches clusters whose names carry the Deployora prefix, and it never
// reads the user's default kubeconfig.
package cluster

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
)

// ErrInvalid is wrapped by every error that rejects a cluster request before
// anything is created or deleted.
var ErrInvalid = errors.New("invalid cluster request")

// ClusterSpec describes a cluster to create.
type ClusterSpec struct {
	// Name must pass CheckName.
	Name string
	// NodeImage is the pinned Kind node image (a tag other than latest, or a digest).
	NodeImage string
	// KubeconfigPath is the private kubeconfig for this run, never the user's own.
	KubeconfigPath string
}

// Validate rejects a spec that must never reach a provider.
func (s ClusterSpec) Validate() error {
	if err := CheckName(s.Name); err != nil {
		return err
	}
	if !isPinned(s.NodeImage) {
		return fmt.Errorf("%w: node image %q must be pinned with a tag other than latest, or a digest", ErrInvalid, s.NodeImage)
	}
	return checkKubeconfigPath(s.KubeconfigPath)
}

// Cluster is a cluster created by a provider.
type Cluster struct {
	Name           string
	KubeconfigPath string
}

// ClusterInfo describes an existing cluster, as listed for garbage collection.
type ClusterInfo struct {
	Name string
}

// ClusterProvider creates and deletes clusters. Kind is the only implementation
// in v0.1; the interface exists so the engine can be tested with a fake.
type ClusterProvider interface {
	Create(ctx context.Context, spec ClusterSpec) (Cluster, error)
	Delete(ctx context.Context, c Cluster) error
	// List returns only clusters whose names carry the Deployora prefix.
	List(ctx context.Context) ([]ClusterInfo, error)
}

func checkKubeconfigPath(p string) error {
	if p == "" || !filepath.IsAbs(p) {
		return fmt.Errorf("%w: kubeconfig path %q must be absolute", ErrInvalid, p)
	}
	if filepath.Base(filepath.Dir(p)) == ".kube" {
		return fmt.Errorf("%w: kubeconfig path %q looks like a user's default kubeconfig", ErrInvalid, p)
	}
	return nil
}

// isPinned reports whether a container image reference carries a digest or a
// tag other than latest. A registry port ("host:5000/image") is not a tag.
func isPinned(image string) bool {
	if strings.Contains(image, "@") {
		return true
	}
	i := strings.LastIndex(image, ":")
	if i < 0 || i < strings.LastIndex(image, "/") {
		return false
	}
	tag := image[i+1:]
	return tag != "" && tag != "latest"
}
