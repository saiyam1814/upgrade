package state

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// helper: point Dir() at a tmpdir for the test.
func withTmpDir(t *testing.T) string {
	t.Helper()
	d := t.TempDir()
	t.Setenv("KUBECTL_UPGRADE_STATE_DIR", d)
	return d
}

func TestDir_RespectsEnvOverride(t *testing.T) {
	d := withTmpDir(t)
	got, err := Dir()
	if err != nil {
		t.Fatalf("Dir: %v", err)
	}
	if got != d {
		t.Errorf("Dir env override ignored: got %q want %q", got, d)
	}
	if _, err := os.Stat(d); err != nil {
		t.Errorf("Dir should ensure path exists: %v", err)
	}
}

func TestAppendRun_AndLatestRun(t *testing.T) {
	withTmpDir(t)
	now := time.Now()
	if err := AppendRun(Run{Command: "preflight", Target: "v1.34", Context: "prod-east", StartedAt: now.Add(-time.Hour)}); err != nil {
		t.Fatalf("AppendRun: %v", err)
	}
	if err := AppendRun(Run{Command: "preflight", Target: "v1.34", Context: "prod-east", StartedAt: now}); err != nil {
		t.Fatalf("AppendRun: %v", err)
	}
	if err := AppendRun(Run{Command: "scan", Target: "v1.34", Context: "prod-east", StartedAt: now}); err != nil {
		t.Fatalf("AppendRun: %v", err)
	}

	r, ok := LatestRun("preflight", "prod-east")
	if !ok {
		t.Fatal("LatestRun: nothing found")
	}
	if !r.StartedAt.Equal(now) {
		t.Errorf("LatestRun returned older row: got %v want %v", r.StartedAt, now)
	}
	// command filter
	r2, ok := LatestRun("scan", "prod-east")
	if !ok || r2.Command != "scan" {
		t.Errorf("LatestRun(scan): got %+v ok=%v", r2, ok)
	}
}

func TestSaveLoadWave(t *testing.T) {
	withTmpDir(t)
	w := &Wave{
		ID:         "wave-1",
		HostTarget: "v1.34",
		StartedAt:  time.Now(),
		Tenants: []TenantRow{
			{Namespace: "ns-a", Name: "tenant-a", Status: TenantPending},
			{Namespace: "ns-b", Name: "tenant-b", Status: TenantUpgrading},
		},
	}
	if err := SaveWave(w); err != nil {
		t.Fatalf("SaveWave: %v", err)
	}
	got, err := LoadWave("wave-1")
	if err != nil {
		t.Fatalf("LoadWave: %v", err)
	}
	if got.ID != w.ID || len(got.Tenants) != 2 {
		t.Errorf("LoadWave round-trip mismatch: %+v", got)
	}
}

func TestMostRecentIncompleteWave(t *testing.T) {
	d := withTmpDir(t)
	// Create one done wave, one incomplete; expect the incomplete one.
	doneW := &Wave{ID: "done", HostTarget: "v1.34", StartedAt: time.Now().Add(-time.Hour),
		Tenants: []TenantRow{{Status: TenantDone}}}
	if err := SaveWave(doneW); err != nil {
		t.Fatal(err)
	}
	pendW := &Wave{ID: "pending", HostTarget: "v1.34", StartedAt: time.Now(),
		Tenants: []TenantRow{{Status: TenantPending}}}
	if err := SaveWave(pendW); err != nil {
		t.Fatal(err)
	}
	got, err := MostRecentIncompleteWave()
	if err != nil {
		t.Fatalf("MostRecentIncompleteWave: %v", err)
	}
	if got == nil || got.ID != "pending" {
		t.Errorf("expected the pending wave; got %+v", got)
	}
	// sanity: directory layout
	if _, err := os.Stat(filepath.Join(d, "waves", "done.json")); err != nil {
		t.Errorf("done.json should exist on disk: %v", err)
	}
}

func TestDiffNew(t *testing.T) {
	prev := map[string]bool{"a": true, "b": true}
	cur := map[string]bool{"b": true, "c": true, "d": true}
	got := DiffNew(cur, prev)
	if len(got) != 2 || got[0] != "c" || got[1] != "d" {
		t.Errorf("DiffNew: got %v want [c d]", got)
	}
}
