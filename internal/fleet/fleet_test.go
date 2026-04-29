package fleet

import (
	"context"
	"reflect"
	"sort"
	"testing"

	"github.com/saiyam1814/upgrade/internal/finding"
)

func TestFilterExcluded(t *testing.T) {
	in := []string{"prod-east", "prod-west", "staging", "scratch-1", "sandbox-foo"}
	got := filterExcluded(in, []string{"sandbox", "scratch"})
	want := []string{"prod-east", "prod-west", "staging"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("filterExcluded: got %v, want %v", got, want)
	}
}

func TestFilterExcluded_NoExcludes(t *testing.T) {
	in := []string{"a", "b"}
	got := filterExcluded(in, nil)
	if !reflect.DeepEqual(got, in) {
		t.Errorf("filterExcluded with nil excludes should pass through; got %v", got)
	}
}

func TestVersionDistribution(t *testing.T) {
	results := []ContextResult{
		{Context: "a", ServerVersion: "v1.31.2"},
		{Context: "b", ServerVersion: "v1.31.4"},
		{Context: "c", ServerVersion: "v1.30.4-gke.1234"},
		{Context: "d", ServerVersion: "v1.30.4"},
		{Context: "e", ServerVersion: ""},
	}
	got := VersionDistribution(results)
	if got["v1.31"] != 2 {
		t.Errorf("v1.31 should have 2 clusters, got %d", got["v1.31"])
	}
	if got["v1.30"] != 2 {
		t.Errorf("v1.30 should have 2 clusters (one with -gke suffix); got %d", got["v1.30"])
	}
	if got["unknown"] != 1 {
		t.Errorf("empty server version should bucket as 'unknown'; got %d", got["unknown"])
	}
}

func TestNormalizeMinor(t *testing.T) {
	tests := []struct{ in, out string }{
		{"v1.31.2", "v1.31"},
		{"1.31.2", "v1.31"},
		{"v1.30.4-gke.1234", "v1.30"},
		{"v1.32.0+rke2", "v1.32"},
		{"", ""},
		{"weird", ""},
	}
	for _, tt := range tests {
		got := normalizeMinor(tt.in)
		if got != tt.out {
			t.Errorf("normalizeMinor(%q): got %q, want %q", tt.in, got, tt.out)
		}
	}
}

func TestAggregateCounts(t *testing.T) {
	results := []ContextResult{
		{Findings: []finding.Finding{
			{Severity: finding.Blocker},
			{Severity: finding.High},
			{Severity: finding.Medium},
		}},
		{Findings: []finding.Finding{
			{Severity: finding.Blocker},
			{Severity: finding.High},
		}},
	}
	got := AggregateCounts(results)
	if got["BLOCKER"] != 2 {
		t.Errorf("BLOCKER count: got %d, want 2", got["BLOCKER"])
	}
	if got["HIGH"] != 2 {
		t.Errorf("HIGH count: got %d, want 2", got["HIGH"])
	}
	if got["MEDIUM"] != 1 {
		t.Errorf("MEDIUM count: got %d, want 1", got["MEDIUM"])
	}
}

func TestFindingDistribution_GroupsAcrossClusters(t *testing.T) {
	// Same finding type appears in 3 contexts → distribution shows 3.
	mk := func(title string) finding.Finding {
		return finding.Finding{Severity: finding.Blocker, Title: title}
	}
	results := []ContextResult{
		{Context: "a", Findings: []finding.Finding{mk("PDB stuck (target v1.34)")}},
		{Context: "b", Findings: []finding.Finding{mk("PDB stuck (target v1.34)")}},
		{Context: "c", Findings: []finding.Finding{mk("PDB stuck (target v1.34)")}},
		{Context: "d", Findings: []finding.Finding{mk("Different finding")}},
	}
	got := FindingDistribution(results)
	// Title with "(target ..." suffix is stripped during grouping.
	for k, v := range got {
		sort.Strings(v)
		got[k] = v
	}
	pdbKey := "BLOCKER: PDB stuck"
	if len(got[pdbKey]) != 3 {
		t.Errorf("PDB finding should appear in 3 contexts; got %d (key=%q dist=%v)", len(got[pdbKey]), pdbKey, got)
	}
}

func TestRun_ContextRequired(t *testing.T) {
	_, err := Run(testCtx(), Options{}, nil)
	if err == nil {
		t.Errorf("Run with no contexts and no AllContexts should error")
	}
}

func testCtx() context.Context { return context.Background() }
