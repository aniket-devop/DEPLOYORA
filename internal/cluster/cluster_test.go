package cluster

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestCheckName(t *testing.T) {
	good := []string{
		"deployora-abc123", "deployora-spike-1a2b3c", "deployora-0",
		"deployora-" + strings.Repeat("a", 30),
	}
	for _, name := range good {
		if err := CheckName(name); err != nil {
			t.Errorf("%q: unexpected error: %v", name, err)
		}
	}
	bad := []string{
		"", "kind", "docker-desktop", "deployora", "deployora-", "xdeployora-abc",
		"Deployora-abc", "deployora-ABC", "deployora-a b", "deployora-a/b",
		"deployora-a_b", "deployora-abc-", "deployora-" + strings.Repeat("a", 40),
	}
	for _, name := range bad {
		if err := CheckName(name); !errors.Is(err, ErrInvalid) {
			t.Errorf("%q: got %v, want ErrInvalid", name, err)
		}
	}
}

func TestName(t *testing.T) {
	got, err := Name("abc123")
	if err != nil || got != "deployora-abc123" {
		t.Errorf("Name(abc123) = %q, %v", got, err)
	}
	for _, id := range []string{"", "ABC", "a b", "-"} {
		if _, err := Name(id); !errors.Is(err, ErrInvalid) {
			t.Errorf("Name(%q): got %v, want ErrInvalid", id, err)
		}
	}
}

func TestIsPinned(t *testing.T) {
	tests := []struct {
		image string
		want  bool
	}{
		{"kindest/node:v1.36.1", true},
		{"kindest/node@sha256:abc", true},
		{"kindest/node", false},
		{"kindest/node:latest", false},
		{"localhost:5000/node", false},
		{"localhost:5000/node:v1", true},
		{"", false},
	}
	for _, tt := range tests {
		if got := isPinned(tt.image); got != tt.want {
			t.Errorf("isPinned(%q) = %v, want %v", tt.image, got, tt.want)
		}
	}
}

func TestSpecValidate(t *testing.T) {
	valid := ClusterSpec{
		Name:           "deployora-abc123",
		NodeImage:      "kindest/node:v1.36.1",
		KubeconfigPath: filepath.Join(t.TempDir(), "kubeconfig"),
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid spec rejected: %v", err)
	}
	tests := []struct {
		name   string
		mutate func(*ClusterSpec)
	}{
		{"name without prefix", func(s *ClusterSpec) { s.Name = "kind" }},
		{"empty image", func(s *ClusterSpec) { s.NodeImage = "" }},
		{"image without tag", func(s *ClusterSpec) { s.NodeImage = "kindest/node" }},
		{"latest image", func(s *ClusterSpec) { s.NodeImage = "kindest/node:latest" }},
		{"relative kubeconfig", func(s *ClusterSpec) { s.KubeconfigPath = "kubeconfig" }},
		{"empty kubeconfig", func(s *ClusterSpec) { s.KubeconfigPath = "" }},
		{"default-looking kubeconfig", func(s *ClusterSpec) {
			s.KubeconfigPath = filepath.Join(t.TempDir(), ".kube", "config")
		}},
	}
	for _, tt := range tests {
		s := valid
		tt.mutate(&s)
		if err := s.Validate(); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: got %v, want ErrInvalid", tt.name, err)
		}
	}
}

func TestNewWorkDir(t *testing.T) {
	w, err := NewWorkDir(t.TempDir())
	if err != nil {
		t.Fatalf("NewWorkDir: %v", err)
	}
	if filepath.Dir(w.Kubeconfig) != w.Dir {
		t.Errorf("kubeconfig %q is not inside %q", w.Kubeconfig, w.Dir)
	}
	if err := checkKubeconfigPath(w.Kubeconfig); err != nil {
		t.Errorf("kubeconfig path rejected: %v", err)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(w.Dir)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o700 {
			t.Errorf("directory mode %v, want 0700", info.Mode().Perm())
		}
	}
	if err := w.Remove(); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := os.Stat(w.Dir); !os.IsNotExist(err) {
		t.Errorf("directory still exists after Remove: %v", err)
	}
	if err := w.Remove(); err != nil {
		t.Errorf("second Remove: %v", err)
	}
}
