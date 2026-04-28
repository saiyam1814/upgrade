// Package apis owns the deprecated-API rule set. The data is sourced
// from FairwindsOps/pluto's versions.yaml (Apache 2.0) embedded at
// build time. Lookups are cheap; the dataset is hot in memory.
package apis

import (
	_ "embed"
	"fmt"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/saiyam1814/upgrade/internal/finding"
)

//go:embed versions.yaml
var rawVersions []byte

// Rule is one row of pluto's versions.yaml.
type Rule struct {
	APIVersion             string `yaml:"version"`
	Kind                   string `yaml:"kind"`
	DeprecatedIn           string `yaml:"deprecated-in"`
	RemovedIn              string `yaml:"removed-in"`
	ReplacementAPI         string `yaml:"replacement-api"`
	ReplacementAvailableIn string `yaml:"replacement-available-in"`
	Component              string `yaml:"component"`
}

type fileShape struct {
	Deprecated []Rule            `yaml:"deprecated-versions"`
	Targets    map[string]string `yaml:"target-versions,omitempty"`
}

// DataPath identifies the embedded data origin in version output.
const DataPath = "github.com/FairwindsOps/pluto:versions.yaml"

// Engine is the loaded rule set with fast lookup indexes.
type Engine struct {
	all   []Rule
	byGVK map[string][]Rule // key = "<apiVersion>|<Kind>"
}

// Load parses the embedded ruleset. Returns an error only if the
// vendored data is corrupt — should never happen at runtime.
func Load() (*Engine, error) {
	var f fileShape
	if err := yaml.Unmarshal(rawVersions, &f); err != nil {
		return nil, fmt.Errorf("decode versions.yaml: %w", err)
	}
	e := &Engine{all: f.Deprecated, byGVK: map[string][]Rule{}}
	for _, r := range f.Deprecated {
		k := key(r.APIVersion, r.Kind)
		e.byGVK[k] = append(e.byGVK[k], r)
	}
	return e, nil
}

func key(apiVersion, kind string) string {
	return strings.ToLower(apiVersion) + "|" + strings.ToLower(kind)
}

// Lookup returns every rule matching a (apiVersion, kind) pair.
// May return multiple — pluto's data sometimes has component-specific
// duplicates (cert-manager + k8s rows for the same Kind).
func (e *Engine) Lookup(apiVersion, kind string) []Rule {
	return e.byGVK[key(apiVersion, kind)]
}

// FindingFor produces a finding for the given object if any rule
// applies on the source→target version pair. Returns nil when the
// object is safe.
func (e *Engine) FindingFor(obj finding.Object, src finding.Source, target Semver) *finding.Finding {
	for _, r := range e.Lookup(obj.APIVersion, obj.Kind) {
		f := ruleToFinding(r, obj, src, target)
		if f != nil {
			return f
		}
	}
	return nil
}

func ruleToFinding(r Rule, obj finding.Object, src finding.Source, target Semver) *finding.Finding {
	removed, removedOK := Parse(r.RemovedIn)
	deprecated, deprecatedOK := Parse(r.DeprecatedIn)

	switch {
	case removedOK && !target.Less(removed):
		// Target version is at or past the removal — BLOCKER.
		return &finding.Finding{
			Severity:     finding.Blocker,
			Category:     finding.CategoryAPI,
			Title:        fmt.Sprintf("%s/%s removed in %s (target %s)", r.APIVersion, r.Kind, r.RemovedIn, target),
			Detail:       deprecationDetail(r),
			Source:       src,
			Object:       &obj,
			Fix:          fixHint(r),
			DeprecatedIn: r.DeprecatedIn,
			RemovedIn:    r.RemovedIn,
			Replacement:  r.ReplacementAPI,
			Docs:         deprecationDocs(),
		}
	case deprecatedOK && !target.Less(deprecated):
		// Target version is at or past deprecation but before removal — HIGH.
		return &finding.Finding{
			Severity:     finding.High,
			Category:     finding.CategoryAPI,
			Title:        fmt.Sprintf("%s/%s deprecated in %s; will be removed in %s", r.APIVersion, r.Kind, r.DeprecatedIn, fallback(r.RemovedIn, "a future release")),
			Detail:       deprecationDetail(r),
			Source:       src,
			Object:       &obj,
			Fix:          fixHint(r),
			DeprecatedIn: r.DeprecatedIn,
			RemovedIn:    r.RemovedIn,
			Replacement:  r.ReplacementAPI,
			Docs:         deprecationDocs(),
		}
	}
	return nil
}

func deprecationDetail(r Rule) string {
	parts := []string{}
	if r.Component != "" {
		parts = append(parts, fmt.Sprintf("component=%s", r.Component))
	}
	if r.ReplacementAPI != "" {
		parts = append(parts, fmt.Sprintf("replacement=%s", r.ReplacementAPI))
		if r.ReplacementAvailableIn != "" {
			parts = append(parts, fmt.Sprintf("(available since %s)", r.ReplacementAvailableIn))
		}
	}
	return strings.Join(parts, " ")
}

func fixHint(r Rule) string {
	if r.ReplacementAPI == "" {
		return "Migrate to a supported alternative; consult release notes for this Kind."
	}
	return fmt.Sprintf("Replace `apiVersion: %s` with `apiVersion: %s` for kind %s. Convert any field renames per the deprecation guide.", r.APIVersion, r.ReplacementAPI, r.Kind)
}

func fallback(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func deprecationDocs() []string {
	return []string{"https://kubernetes.io/docs/reference/using-api/deprecation-guide/"}
}

// All returns every rule (used for `upgrade simulate --explain`).
func (e *Engine) All() []Rule { return e.all }

// Semver is a permissive parse of "v1.X.Y" / "1.X" / "v1.X" /
// "1.X.0-beta". Only major + minor are compared; patch is ignored.
type Semver struct {
	Major int
	Minor int
}

func (s Semver) String() string { return fmt.Sprintf("v%d.%d", s.Major, s.Minor) }

// Less reports whether s < other.
func (s Semver) Less(other Semver) bool {
	if s.Major != other.Major {
		return s.Major < other.Major
	}
	return s.Minor < other.Minor
}

// Equal compares major.minor only.
func (s Semver) Equal(other Semver) bool {
	return s.Major == other.Major && s.Minor == other.Minor
}

// Parse handles "v1.30", "1.30", "v1.30.0", "1.30.4", "v1.32.0-rc.1".
func Parse(in string) (Semver, bool) {
	s := strings.TrimSpace(in)
	s = strings.TrimPrefix(s, "v")
	if s == "" {
		return Semver{}, false
	}
	if i := strings.IndexAny(s, "-+"); i >= 0 {
		s = s[:i]
	}
	parts := strings.SplitN(s, ".", 3)
	if len(parts) < 2 {
		return Semver{}, false
	}
	major, err := strconv.Atoi(parts[0])
	if err != nil {
		return Semver{}, false
	}
	minor, err := strconv.Atoi(parts[1])
	if err != nil {
		return Semver{}, false
	}
	return Semver{Major: major, Minor: minor}, true
}

// MustParse is for hard-coded constants.
func MustParse(in string) Semver {
	v, ok := Parse(in)
	if !ok {
		panic("invalid semver: " + in)
	}
	return v
}
