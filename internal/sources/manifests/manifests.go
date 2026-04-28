// Package manifests reads raw Kubernetes YAML/JSON from disk:
// individual files, directories (recursive), or stdin. It emits
// finding.Object records — bare GVK + namespace/name — that the
// rule engines can scan.
package manifests

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"sigs.k8s.io/yaml"

	"github.com/saiyam1814/upgrade/internal/finding"
)

// Object is a parsed manifest object plus its source location for
// rendering ("docs/cronjob.yaml:doc#2").
type Object struct {
	Obj    finding.Object
	Source finding.Source
}

// ParseString parses an in-memory YAML/JSON string (multi-doc OK) into
// objects, tagging each with the given source location.
func ParseString(content, location string) ([]Object, error) {
	return readStream(strings.NewReader(content), location)
}

// Read scans a path (file or directory) and returns every Kubernetes
// object found. Non-K8s YAML (Helm Chart.yaml, kustomization.yaml,
// arbitrary config) is silently skipped.
func Read(root string) ([]Object, error) {
	if root == "-" {
		return readStream(os.Stdin, "stdin")
	}
	info, err := os.Stat(root)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return readFile(root)
	}
	var out []Object
	walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if isVendorish(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if !isYAMLOrJSON(path) {
			return nil
		}
		objs, err := readFile(path)
		if err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		out = append(out, objs...)
		return nil
	})
	if walkErr != nil {
		return nil, walkErr
	}
	return out, nil
}

func isVendorish(name string) bool {
	switch name {
	case ".git", "node_modules", "vendor", ".terraform", ".helm", "charts":
		return true
	}
	return strings.HasPrefix(name, ".")
}

func isYAMLOrJSON(path string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	return ext == ".yaml" || ext == ".yml" || ext == ".json"
}

func readFile(path string) ([]Object, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return readStream(f, path)
}

// minimal is the bare shape we need from each YAML doc.
type minimal struct {
	APIVersion string `json:"apiVersion,omitempty"`
	Kind       string `json:"kind,omitempty"`
	Metadata   struct {
		Name      string `json:"name,omitempty"`
		Namespace string `json:"namespace,omitempty"`
	} `json:"metadata,omitempty"`
}

func readStream(r io.Reader, location string) ([]Object, error) {
	var out []Object
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)
	scanner.Split(splitYAMLDocs)
	doc := 0
	for scanner.Scan() {
		doc++
		raw := strings.TrimSpace(scanner.Text())
		if raw == "" {
			continue
		}
		// sigs.k8s.io/yaml parses both YAML and JSON via JSON shape.
		var m minimal
		if err := yaml.Unmarshal([]byte(raw), &m); err != nil {
			continue
		}
		if m.APIVersion == "" || m.Kind == "" {
			continue
		}
		obj := finding.Object{
			APIVersion: m.APIVersion,
			Kind:       m.Kind,
			Namespace:  m.Metadata.Namespace,
			Name:       m.Metadata.Name,
		}
		out = append(out, Object{
			Obj:    obj,
			Source: finding.Source{Kind: "manifest", Location: fmt.Sprintf("%s#doc%d", location, doc)},
		})
	}
	if err := scanner.Err(); err != nil && !errors.Is(err, io.EOF) {
		return out, err
	}
	return out, nil
}

// splitYAMLDocs splits on YAML document separators ("---" on its own line).
func splitYAMLDocs(data []byte, atEOF bool) (advance int, token []byte, err error) {
	if atEOF && len(data) == 0 {
		return 0, nil, nil
	}
	const sep = "\n---"
	i := strings.Index(string(data), sep)
	if i < 0 {
		if atEOF {
			return len(data), data, nil
		}
		return 0, nil, nil
	}
	// Consume up through the newline after "---".
	end := i + len(sep)
	if nl := strings.Index(string(data[end:]), "\n"); nl >= 0 {
		end += nl + 1
	} else if !atEOF {
		return 0, nil, nil
	}
	return end, data[:i], nil
}
