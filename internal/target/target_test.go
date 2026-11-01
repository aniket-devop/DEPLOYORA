package target

import (
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

const deployment = "apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: app\n"
const service = "apiVersion: v1\nkind: Service\nmetadata:\n  name: app\n"
const chartYAML = "apiVersion: v2\nname: app\nversion: 0.1.0\n"
const chartWithRedis = chartYAML + "dependencies:\n  - name: redis\n    version: ~17.0.0\n    repository: https://example.invalid/charts\n"

func write(t *testing.T, root, rel, content string) string {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func mustInvalid(t *testing.T, path string, wantInMessage ...string) {
	t.Helper()
	_, err := Detect(path)
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("Detect(%s): got %v, want ErrInvalid", path, err)
	}
	for _, w := range wantInMessage {
		if !strings.Contains(err.Error(), w) {
			t.Errorf("error %q does not contain %q", err, w)
		}
	}
}

func TestHelmChart(t *testing.T) {
	root := t.TempDir()
	write(t, root, "Chart.yaml", chartYAML)
	write(t, root, "templates/deployment.yaml", "kind: {{ .Values.kind }}\n") // a template, never parsed as YAML
	got, err := Detect(root)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if got.Kind != KindHelmChart || got.ChartName != "app" || got.Path != root {
		t.Errorf("got %+v", got)
	}
}

func TestLibraryChart(t *testing.T) {
	root := t.TempDir()
	write(t, root, "Chart.yaml", chartYAML+"type: library\n")
	mustInvalid(t, root, "library chart")
}

func TestChartWithoutName(t *testing.T) {
	root := t.TempDir()
	write(t, root, "Chart.yaml", "apiVersion: v2\nversion: 0.1.0\n")
	mustInvalid(t, root, "no name")
}

func TestUnvendoredDependencies(t *testing.T) {
	root := t.TempDir()
	write(t, root, "Chart.yaml", chartWithRedis)
	mustInvalid(t, root, "not vendored", "redis", "helm dependency build "+root)
}

func TestVendoredDependencies(t *testing.T) {
	tests := []struct {
		name string
		file string
		ok   bool
	}{
		{"packaged", "charts/redis-17.0.1.tgz", true},
		{"packaged with v prefix", "charts/redis-v17.0.1.tgz", true},
		{"unpacked", "charts/redis/Chart.yaml", true},
		{"another chart with the same prefix", "charts/redis-cluster-1.0.0.tgz", false},
		{"unrelated file", "charts/README.md", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			write(t, root, "Chart.yaml", chartWithRedis)
			write(t, root, tt.file, "x")
			_, err := Detect(root)
			if tt.ok && err != nil {
				t.Errorf("got %v, want no error", err)
			}
			if !tt.ok && !errors.Is(err, ErrInvalid) {
				t.Errorf("got %v, want ErrInvalid", err)
			}
		})
	}
}

func TestDependencyNameWithPathSeparatorIsNeverVendored(t *testing.T) {
	root := t.TempDir()
	write(t, root, "Chart.yaml", chartYAML+"dependencies:\n  - name: ../evil\n")
	mustInvalid(t, root, "not vendored")
}

func TestRequirementsYAML(t *testing.T) {
	root := t.TempDir()
	write(t, root, "Chart.yaml", "apiVersion: v1\nname: app\nversion: 0.1.0\n")
	write(t, root, "requirements.yaml", "dependencies:\n  - name: redis\n    version: 1.0.0\n")
	mustInvalid(t, root, "not vendored", "redis")
}

func TestManifestsDirectory(t *testing.T) {
	root := t.TempDir()
	write(t, root, "b/deployment.yaml", "# a comment\n---\n"+deployment+"---\n"+service)
	write(t, root, "a/service.yml", service)
	write(t, root, "values.yaml", "replicas: 2\n")
	write(t, root, ".git/hidden.yaml", deployment)
	got, err := Detect(root)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if got.Kind != KindManifests || got.Path != root || len(got.Files) != 2 || !sort.StringsAreSorted(got.Files) {
		t.Errorf("got %+v", got)
	}
}

func TestSingleManifestFile(t *testing.T) {
	root := t.TempDir()
	f := write(t, root, "app.yaml", deployment)
	got, err := Detect(f)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if got.Kind != KindManifests || len(got.Files) != 1 || got.Files[0] != f {
		t.Errorf("got %+v", got)
	}
}

func TestEmptyDirectory(t *testing.T) {
	mustInvalid(t, t.TempDir(), "no Kubernetes YAML")
}

func TestOnlyNonKubernetesYAML(t *testing.T) {
	root := t.TempDir()
	write(t, root, "values.yaml", "replicas: 2\n")
	write(t, root, "list.yaml", "- a\n- b\n")
	mustInvalid(t, root, "no Kubernetes YAML")
}

func TestOnlyHiddenManifests(t *testing.T) {
	root := t.TempDir()
	write(t, root, ".hidden/app.yaml", deployment)
	mustInvalid(t, root, "no Kubernetes YAML")
}

func TestSeveralCharts(t *testing.T) {
	root := t.TempDir()
	write(t, root, "a/Chart.yaml", chartYAML)
	write(t, root, "b/Chart.yaml", chartYAML)
	mustInvalid(t, root, "2 charts", "one chart at a time")
}

func TestOneChartInASubdirectory(t *testing.T) {
	root := t.TempDir()
	write(t, root, "app/Chart.yaml", chartYAML)
	mustInvalid(t, root, "did you mean", filepath.Join(root, "app"))
}

func TestMissingPath(t *testing.T) {
	mustInvalid(t, filepath.Join(t.TempDir(), "nope"), "does not exist")
}

func TestChartYAMLFileGiven(t *testing.T) {
	root := t.TempDir()
	f := write(t, root, "Chart.yaml", chartYAML)
	mustInvalid(t, f, "chart directory")
}

func TestNotAYAMLFile(t *testing.T) {
	root := t.TempDir()
	f := write(t, root, "README.md", "hello")
	mustInvalid(t, f, "not a YAML file")
}

func TestKustomize(t *testing.T) {
	root := t.TempDir()
	write(t, root, "kustomization.yaml", "resources:\n  - deployment.yaml\n")
	write(t, root, "deployment.yaml", deployment)
	mustInvalid(t, root, "kustomize")

	only := t.TempDir()
	f := write(t, only, "kustomization.yaml", "resources: []\n")
	mustInvalid(t, f, "kustomize")

	other := t.TempDir()
	write(t, other, "overlay.yaml", "apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\n")
	mustInvalid(t, other, "kustomize")
}

func TestUnparseableYAML(t *testing.T) {
	root := t.TempDir()
	write(t, root, "bad.yaml", "a: [1, 2\n")
	mustInvalid(t, root, "bad.yaml")
}
