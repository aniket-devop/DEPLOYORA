// Package target decides what the path given to "deployora run" is: a Helm
// chart or Kubernetes manifests. It only reads files. It never runs Helm and
// never touches a cluster.
//
// The rules come from docs/architecture/overview.md section 15.1. Anything it
// cannot classify with confidence is rejected with a message that names the
// problem and the fix, and the caller turns that into invalid_config.
package target

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"
)

// maxFileSize caps every YAML file that is read, in bytes.
const maxFileSize = 10 << 20

// Kind is what a target turned out to be.
type Kind string

const (
	KindHelmChart Kind = "helm_chart"
	KindManifests Kind = "manifests"
)

// Target is a classified path.
type Target struct {
	Kind Kind
	// Path is absolute: the chart directory, or the directory or file given for manifests.
	Path string
	// ChartName is set for a Helm chart.
	ChartName string
	// Files lists the Kubernetes manifest files (absolute, sorted) for manifests.
	Files []string
}

// ErrInvalid is wrapped by every error that means "the target is not usable".
// Other errors (for example an unreadable directory) are environment problems.
var ErrInvalid = errors.New("invalid target")

func invalidf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, args...))
}

// Detect classifies path.
func Detect(path string) (Target, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return Target{}, err
	}
	info, err := os.Stat(abs)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return Target{}, invalidf("path %s does not exist", abs)
	case err != nil:
		return Target{}, err
	case info.IsDir():
		return detectDir(abs)
	default:
		return detectFile(abs)
	}
}

func detectFile(file string) (Target, error) {
	base := filepath.Base(file)
	if base == "Chart.yaml" {
		return Target{}, invalidf("%s is a Chart.yaml file; pass the chart directory %s instead", file, filepath.Dir(file))
	}
	if !isYAML(base) {
		return Target{}, invalidf("%s is not a YAML file; pass a chart directory, a directory of manifests or a manifest .yaml file", file)
	}
	if isKustomization(base) {
		return Target{}, kustomizeError(file)
	}
	objs, err := readObjects(file)
	if err != nil {
		return Target{}, err
	}
	for _, o := range objs {
		if o.isKustomize() {
			return Target{}, kustomizeError(file)
		}
	}
	if len(objs) == 0 {
		return Target{}, invalidf("%s contains no Kubernetes objects (a document with apiVersion and kind)", file)
	}
	return Target{Kind: KindManifests, Path: file, Files: []string{file}}, nil
}

func detectDir(dir string) (Target, error) {
	if isFile(filepath.Join(dir, "Chart.yaml")) {
		return detectChart(dir)
	}

	var charts, yamls []string
	walkErr := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if p == dir {
				return nil
			}
			if strings.HasPrefix(d.Name(), ".") || d.Name() == "node_modules" {
				return filepath.SkipDir
			}
			if isFile(filepath.Join(p, "Chart.yaml")) {
				charts = append(charts, p)
				return filepath.SkipDir
			}
			return nil
		}
		if isYAML(d.Name()) {
			yamls = append(yamls, p)
		}
		return nil
	})
	if walkErr != nil {
		return Target{}, walkErr
	}
	sort.Strings(charts)
	sort.Strings(yamls)

	switch {
	case len(charts) > 1:
		return Target{}, invalidf("found %d charts under %s (%s); run one chart at a time, for example with a CI matrix, and pass that chart's directory",
			len(charts), dir, listRelative(dir, charts))
	case len(charts) == 1:
		return Target{}, invalidf("there is no Chart.yaml in %s, but a chart exists at %s; did you mean to pass that directory?", dir, charts[0])
	}

	var files []string
	for _, f := range yamls {
		if isKustomization(filepath.Base(f)) {
			return Target{}, kustomizeError(f)
		}
		objs, err := readObjects(f)
		if err != nil {
			return Target{}, err
		}
		for _, o := range objs {
			if o.isKustomize() {
				return Target{}, kustomizeError(f)
			}
		}
		if len(objs) > 0 {
			files = append(files, f)
		}
	}
	if len(files) == 0 {
		return Target{}, invalidf("no Kubernetes YAML (documents with apiVersion and kind) found in %s", dir)
	}
	return Target{Kind: KindManifests, Path: dir, Files: files}, nil
}

type dependency struct {
	Name string `yaml:"name"`
}

type chartFile struct {
	Name         string       `yaml:"name"`
	Type         string       `yaml:"type"`
	Dependencies []dependency `yaml:"dependencies"`
}

type requirementsFile struct {
	Dependencies []dependency `yaml:"dependencies"`
}

func detectChart(dir string) (Target, error) {
	chartPath := filepath.Join(dir, "Chart.yaml")
	var c chartFile
	if err := readYAMLInto(chartPath, &c); err != nil {
		return Target{}, err
	}
	if c.Name == "" {
		return Target{}, invalidf("%s has no name", chartPath)
	}
	if c.Type == "library" {
		return Target{}, invalidf("%s is a library chart; library charts cannot be deployed, so test a chart that uses it", dir)
	}

	deps := c.Dependencies
	if reqPath := filepath.Join(dir, "requirements.yaml"); isFile(reqPath) {
		var r requirementsFile
		if err := readYAMLInto(reqPath, &r); err != nil {
			return Target{}, err
		}
		deps = append(deps, r.Dependencies...)
	}
	if missing := missingDependencies(filepath.Join(dir, "charts"), deps); len(missing) > 0 {
		return Target{}, invalidf("chart dependencies are not vendored in %s: %s; run: helm dependency build %s",
			filepath.Join(dir, "charts"), strings.Join(missing, ", "), shellArg(dir))
	}
	return Target{Kind: KindHelmChart, Path: dir, ChartName: c.Name}, nil
}

func missingDependencies(chartsDir string, deps []dependency) []string {
	seen := map[string]bool{}
	var missing []string
	for _, d := range deps {
		if d.Name == "" || seen[d.Name] {
			continue
		}
		seen[d.Name] = true
		if !vendored(chartsDir, d.Name) {
			missing = append(missing, d.Name)
		}
	}
	sort.Strings(missing)
	return missing
}

// vendored reports whether a dependency is present in the charts directory,
// either unpacked ("charts/<name>/") or packaged ("charts/<name>-<version>.tgz").
// The version is not compared: Chart.yaml may hold a range such as ~1.2.0 while
// the archive holds the resolved version.
func vendored(chartsDir, name string) bool {
	if strings.ContainsAny(name, "/\\") || name == ".." {
		return false
	}
	if info, err := os.Stat(filepath.Join(chartsDir, name)); err == nil && info.IsDir() {
		return true
	}
	entries, err := os.ReadDir(chartsDir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || !strings.HasPrefix(n, name+"-") || !strings.HasSuffix(n, ".tgz") {
			continue
		}
		if startsWithVersion(n[len(name)+1:]) {
			return true
		}
	}
	return false
}

func startsWithVersion(s string) bool {
	if s == "" {
		return false
	}
	if s[0] >= '0' && s[0] <= '9' {
		return true
	}
	return s[0] == 'v' && len(s) > 1 && s[1] >= '0' && s[1] <= '9'
}

type object struct {
	APIVersion string
	Kind       string
}

func (o object) isKustomize() bool {
	return strings.HasPrefix(o.APIVersion, "kustomize.config.k8s.io/")
}

// readObjects returns the Kubernetes objects (documents that have both
// apiVersion and kind) of a YAML file. A file without any is not an error.
func readObjects(file string) ([]object, error) {
	data, err := readFile(file)
	if err != nil {
		return nil, err
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	var objs []object
	for {
		var doc yaml.Node
		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			return objs, nil
		}
		if err != nil {
			return nil, invalidf("cannot parse %s as YAML: %v", file, err)
		}
		if o, ok := objectFrom(&doc); ok {
			objs = append(objs, o)
		}
	}
}

func objectFrom(doc *yaml.Node) (object, bool) {
	n := doc
	if n.Kind == yaml.DocumentNode {
		if len(n.Content) == 0 {
			return object{}, false
		}
		n = n.Content[0]
	}
	if n.Kind != yaml.MappingNode {
		return object{}, false
	}
	var o object
	for i := 0; i+1 < len(n.Content); i += 2 {
		k, v := n.Content[i], n.Content[i+1]
		if v.Kind != yaml.ScalarNode {
			continue
		}
		switch k.Value {
		case "apiVersion":
			o.APIVersion = v.Value
		case "kind":
			o.Kind = v.Value
		}
	}
	return o, o.APIVersion != "" && o.Kind != ""
}

func readYAMLInto(file string, v any) error {
	data, err := readFile(file)
	if err != nil {
		return err
	}
	if err := yaml.Unmarshal(data, v); err != nil {
		return invalidf("cannot parse %s: %v", file, err)
	}
	return nil
}

func readFile(file string) ([]byte, error) {
	info, err := os.Stat(file)
	if err != nil {
		return nil, err
	}
	if info.Size() > maxFileSize {
		return nil, invalidf("%s is larger than %d MiB", file, maxFileSize>>20)
	}
	return os.ReadFile(file)
}

func kustomizeError(file string) error {
	return invalidf("%s is a kustomize file; kustomize is not supported in v0.1.0, so pass rendered manifests (for example the output of kustomize build) as a .yaml file or directory", file)
}

func isKustomization(base string) bool {
	return base == "kustomization.yaml" || base == "kustomization.yml"
}

func isYAML(name string) bool {
	ext := strings.ToLower(filepath.Ext(name))
	return ext == ".yaml" || ext == ".yml"
}

func isFile(p string) bool {
	info, err := os.Stat(p)
	return err == nil && info.Mode().IsRegular()
}

// shellArg quotes a path for display in a command line when it has spaces.
func shellArg(s string) string {
	if strings.ContainsAny(s, " \t") {
		return `"` + s + `"`
	}
	return s
}

func listRelative(base string, paths []string) string {
	const limit = 5
	var out []string
	for i, p := range paths {
		if i == limit {
			out = append(out, "...")
			break
		}
		if rel, err := filepath.Rel(base, p); err == nil {
			p = rel
		}
		out = append(out, p)
	}
	return strings.Join(out, ", ")
}
