// Package report renders a slice of finding.Finding into one of:
// human-friendly terminal output, JSON, SARIF (for CI integrations),
// or Markdown (for posting in PRs / issues).
package report

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/saiyam1814/upgrade/internal/finding"
)

type Format string

const (
	FormatHuman    Format = "human"
	FormatJSON     Format = "json"
	FormatMarkdown Format = "md"
	FormatSARIF    Format = "sarif"
)

// Header is the metadata included in every report.
type Header struct {
	Tool          string `json:"tool"`
	ToolVersion   string `json:"toolVersion"`
	Source        string `json:"source"`
	SourceVersion string `json:"sourceVersion,omitempty"`
	Target        string `json:"target,omitempty"`
	RulesData     string `json:"rulesData,omitempty"`
}

// Render writes findings to w in the requested format.
func Render(w io.Writer, h Header, findings []finding.Finding, f Format) error {
	finding.Sort(findings)
	findings = finding.Dedupe(findings)
	switch f {
	case FormatJSON, "":
		// Default JSON if format is empty (machine consumers).
		if f == "" {
			f = FormatHuman
		}
	}
	switch f {
	case FormatJSON:
		return renderJSON(w, h, findings)
	case FormatMarkdown:
		return renderMarkdown(w, h, findings)
	case FormatSARIF:
		return renderSARIF(w, h, findings)
	case FormatHuman:
		fallthrough
	default:
		return renderHuman(w, h, findings)
	}
}

// ---- human ----

func renderHuman(w io.Writer, h Header, findings []finding.Finding) error {
	fmt.Fprintf(w, "=== %s — source: %s → target %s ===\n",
		h.Tool, prettySource(h), prettyTarget(h))
	if h.RulesData != "" {
		fmt.Fprintf(w, "rules: %s\n", h.RulesData)
	}
	fmt.Fprintln(w)
	if len(findings) == 0 {
		fmt.Fprintln(w, "✓ No upgrade-blocking issues found.")
		return nil
	}

	counts := finding.Counts(findings)
	fmt.Fprintf(w, "Summary: %d BLOCKER · %d HIGH · %d MEDIUM · %d LOW · %d INFO\n\n",
		counts[finding.Blocker], counts[finding.High], counts[finding.Medium], counts[finding.Low], counts[finding.Info])

	curSev := finding.Severity("")
	for _, f := range findings {
		if f.Severity != curSev {
			curSev = f.Severity
			fmt.Fprintf(w, "%s\n", sevHeader(curSev))
		}
		fmt.Fprintf(w, "  %s %s\n", sevGlyph(f.Severity), f.Title)
		if f.Object != nil && f.Object.String() != "" {
			fmt.Fprintf(w, "      OBJECT:  %s\n", f.Object.String())
		}
		if f.Source.Kind != "" {
			fmt.Fprintf(w, "      SOURCE:  %s (%s)\n", f.Source.Kind, f.Source.Location)
		}
		if f.Owner != nil {
			fmt.Fprintf(w, "      OWNER:   %s %s/%s (%s)\n", f.Owner.Kind, f.Owner.Namespace, f.Owner.Name, f.Owner.Image)
		}
		if f.RemovedIn != "" {
			fmt.Fprintf(w, "      REMOVED: %s\n", f.RemovedIn)
		}
		if f.Replacement != "" {
			fmt.Fprintf(w, "      USE:     %s\n", f.Replacement)
		}
		if f.Detail != "" {
			fmt.Fprintf(w, "      INFO:    %s\n", f.Detail)
		}
		if f.Fix != "" {
			fmt.Fprintf(w, "      FIX:     %s\n", f.Fix)
		}
		for _, d := range f.Docs {
			fmt.Fprintf(w, "      DOCS:    %s\n", d)
		}
		fmt.Fprintln(w)
	}
	return nil
}

func sevHeader(s finding.Severity) string {
	switch s {
	case finding.Blocker:
		return "BLOCKERS"
	case finding.High:
		return "HIGH"
	case finding.Medium:
		return "MEDIUM"
	case finding.Low:
		return "LOW"
	case finding.Info:
		return "INFO"
	}
	return string(s)
}

func sevGlyph(s finding.Severity) string {
	switch s {
	case finding.Blocker:
		return "✗"
	case finding.High:
		return "⚠"
	case finding.Medium:
		return "•"
	case finding.Low:
		return "·"
	}
	return "i"
}

func prettySource(h Header) string {
	if h.Source == "" {
		return "(no source)"
	}
	if h.SourceVersion != "" {
		return fmt.Sprintf("%s %s", h.Source, h.SourceVersion)
	}
	return h.Source
}

func prettyTarget(h Header) string {
	if h.Target == "" {
		return "(none specified)"
	}
	return h.Target
}

// ---- json ----

type jsonReport struct {
	Header   Header            `json:"header"`
	Counts   map[string]int    `json:"counts"`
	Findings []finding.Finding `json:"findings"`
}

func renderJSON(w io.Writer, h Header, findings []finding.Finding) error {
	out := jsonReport{
		Header:   h,
		Counts:   map[string]int{},
		Findings: findings,
	}
	for sev, n := range finding.Counts(findings) {
		out.Counts[string(sev)] = n
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}

// ---- markdown ----

func renderMarkdown(w io.Writer, h Header, findings []finding.Finding) error {
	fmt.Fprintf(w, "# upgrade — %s → %s\n\n", prettySource(h), prettyTarget(h))
	if len(findings) == 0 {
		fmt.Fprintln(w, "_No upgrade-blocking issues found._")
		return nil
	}
	c := finding.Counts(findings)
	fmt.Fprintf(w, "**Summary:** %d BLOCKER · %d HIGH · %d MEDIUM · %d LOW\n\n",
		c[finding.Blocker], c[finding.High], c[finding.Medium], c[finding.Low])

	cur := finding.Severity("")
	for _, f := range findings {
		if f.Severity != cur {
			cur = f.Severity
			fmt.Fprintf(w, "## %s\n\n", sevHeader(cur))
		}
		fmt.Fprintf(w, "### %s\n\n", f.Title)
		if f.Object != nil && f.Object.String() != "" {
			fmt.Fprintf(w, "- **Object:** `%s`\n", f.Object.String())
		}
		if f.Source.Kind != "" {
			fmt.Fprintf(w, "- **Source:** %s — `%s`\n", f.Source.Kind, f.Source.Location)
		}
		if f.RemovedIn != "" {
			fmt.Fprintf(w, "- **Removed in:** %s\n", f.RemovedIn)
		}
		if f.Replacement != "" {
			fmt.Fprintf(w, "- **Use:** `%s`\n", f.Replacement)
		}
		if f.Fix != "" {
			fmt.Fprintf(w, "- **Fix:** %s\n", f.Fix)
		}
		for _, d := range f.Docs {
			fmt.Fprintf(w, "- **Docs:** <%s>\n", d)
		}
		fmt.Fprintln(w)
	}
	return nil
}

// ---- sarif ----
//
// Minimal SARIF 2.1.0 output for CI integrations (GitHub code scanning,
// GitLab Code Quality). Each finding becomes one result; severities map
// to the SARIF "level" enum.

type sarifLog struct {
	Schema  string     `json:"$schema"`
	Version string     `json:"version"`
	Runs    []sarifRun `json:"runs"`
}

type sarifRun struct {
	Tool    sarifTool     `json:"tool"`
	Results []sarifResult `json:"results"`
}

type sarifTool struct {
	Driver sarifDriver `json:"driver"`
}

type sarifDriver struct {
	Name           string      `json:"name"`
	Version        string      `json:"version"`
	InformationURI string      `json:"informationUri"`
	Rules          []sarifRule `json:"rules,omitempty"`
}

type sarifRule struct {
	ID               string         `json:"id"`
	Name             string         `json:"name"`
	ShortDescription sarifMultiText `json:"shortDescription"`
	HelpURI          string         `json:"helpUri,omitempty"`
}

type sarifMultiText struct {
	Text string `json:"text"`
}

type sarifResult struct {
	RuleID    string          `json:"ruleId"`
	Level     string          `json:"level"`
	Message   sarifMultiText  `json:"message"`
	Locations []sarifLocation `json:"locations,omitempty"`
}

type sarifLocation struct {
	PhysicalLocation sarifPhysical `json:"physicalLocation"`
}

type sarifPhysical struct {
	ArtifactLocation sarifArtifact `json:"artifactLocation"`
}

type sarifArtifact struct {
	URI string `json:"uri"`
}

func renderSARIF(w io.Writer, h Header, findings []finding.Finding) error {
	rules := map[string]sarifRule{}
	results := []sarifResult{}
	for _, f := range findings {
		rid := string(f.Category)
		if _, ok := rules[rid]; !ok {
			r := sarifRule{
				ID:               rid,
				Name:             string(f.Category),
				ShortDescription: sarifMultiText{Text: string(f.Category)},
			}
			if len(f.Docs) > 0 {
				r.HelpURI = f.Docs[0]
			}
			rules[rid] = r
		}
		loc := f.Source.Location
		if loc == "" {
			loc = "cluster://" + f.Source.Kind
		}
		results = append(results, sarifResult{
			RuleID:    rid,
			Level:     sarifLevel(f.Severity),
			Message:   sarifMultiText{Text: f.Title + " — " + f.Fix},
			Locations: []sarifLocation{{PhysicalLocation: sarifPhysical{ArtifactLocation: sarifArtifact{URI: loc}}}},
		})
	}
	rulesList := make([]sarifRule, 0, len(rules))
	for _, r := range rules {
		rulesList = append(rulesList, r)
	}
	out := sarifLog{
		Schema:  "https://schemastore.azurewebsites.net/schemas/json/sarif-2.1.0-rtm.5.json",
		Version: "2.1.0",
		Runs: []sarifRun{{
			Tool: sarifTool{Driver: sarifDriver{
				Name:           "upgrade",
				Version:        h.ToolVersion,
				InformationURI: "https://github.com/saiyam1814/upgrade",
				Rules:          rulesList,
			}},
			Results: results,
		}},
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}

func sarifLevel(s finding.Severity) string {
	switch s {
	case finding.Blocker, finding.High:
		return "error"
	case finding.Medium:
		return "warning"
	case finding.Low, finding.Info:
		return "note"
	}
	return "warning"
}

// ParseFormat normalizes a flag value into a Format.
func ParseFormat(s string) (Format, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "human", "text", "tty":
		return FormatHuman, nil
	case "json":
		return FormatJSON, nil
	case "md", "markdown":
		return FormatMarkdown, nil
	case "sarif":
		return FormatSARIF, nil
	}
	return "", fmt.Errorf("unknown format %q (want human|json|md|sarif)", s)
}
