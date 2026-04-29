// Package state persists run history, per-cluster findings, and
// fleet-wave progress to a small JSON store under
// $XDG_DATA_HOME (or ~/.kubectl-upgrade) so commands can:
//
//   - --resume an interrupted fleet wave at the right tenant
//   - diff findings since last scan ("3 new BLOCKERs since yesterday")
//   - feed the recommendation engine ("you fixed X yesterday but it's back")
//
// JSON, not SQLite — small data, single-user, dependency-free, easy to
// inspect. Files are flock'd while writing to keep concurrent runs
// (e.g. fleet --parallel) consistent.
package state

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/saiyam1814/upgrade/internal/finding"
)

const (
	dirEnvOverride = "KUBECTL_UPGRADE_STATE_DIR"
	defaultDirName = "kubectl-upgrade"
)

// Dir returns the effective state directory, creating it if missing.
// Order: $KUBECTL_UPGRADE_STATE_DIR > $XDG_DATA_HOME/kubectl-upgrade
// > ~/.kubectl-upgrade. Errors are returned only if creation fails.
func Dir() (string, error) {
	if v := os.Getenv(dirEnvOverride); v != "" {
		return ensure(v)
	}
	if v := os.Getenv("XDG_DATA_HOME"); v != "" {
		return ensure(filepath.Join(v, defaultDirName))
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return ensure(filepath.Join(home, "."+defaultDirName))
}

func ensure(p string) (string, error) {
	if err := os.MkdirAll(p, 0o755); err != nil {
		return "", err
	}
	return p, nil
}

// ---- run history ----------------------------------------------------

// Run is one invocation of a kubectl-upgrade command.
type Run struct {
	ID            string          `json:"id"`
	Command       string          `json:"command"` // "preflight", "scan", "fleet drift", ...
	Target        string          `json:"target,omitempty"`
	Context       string          `json:"context,omitempty"`  // kubeconfig context (single-cluster runs)
	Contexts      []string        `json:"contexts,omitempty"` // multi-context runs
	StartedAt     time.Time       `json:"startedAt"`
	CompletedAt   *time.Time      `json:"completedAt,omitempty"`
	ExitCode      int             `json:"exitCode"`
	Counts        map[string]int  `json:"counts,omitempty"`
	FindingDigest map[string]bool `json:"findingDigest,omitempty"` // finding ID → present
}

// AppendRun records a completed run. Best-effort: persistence errors
// are warnings, never fatal — a missing state dir mustn't break the CLI.
func AppendRun(r Run) error {
	dir, err := Dir()
	if err != nil {
		return err
	}
	path := filepath.Join(dir, "runs.json")
	mu := lockFor(path)
	mu.Lock()
	defer mu.Unlock()

	var runs []Run
	if b, err := os.ReadFile(path); err == nil && len(b) > 0 {
		_ = json.Unmarshal(b, &runs)
	}
	if r.ID == "" {
		r.ID = fmt.Sprintf("%d", time.Now().UnixNano())
	}
	runs = append(runs, r)
	// keep last 200 runs
	if len(runs) > 200 {
		runs = runs[len(runs)-200:]
	}
	return writeJSON(path, runs)
}

// LatestRun returns the most recent run for (command, contextOrFleet).
// Used by the recommendation engine to compute "since last time".
func LatestRun(command, key string) (*Run, bool) {
	dir, err := Dir()
	if err != nil {
		return nil, false
	}
	b, err := os.ReadFile(filepath.Join(dir, "runs.json"))
	if err != nil {
		return nil, false
	}
	var runs []Run
	if err := json.Unmarshal(b, &runs); err != nil {
		return nil, false
	}
	// newest first
	sort.SliceStable(runs, func(i, j int) bool { return runs[i].StartedAt.After(runs[j].StartedAt) })
	for i := range runs {
		r := &runs[i]
		if r.Command != command {
			continue
		}
		if key != "" {
			match := r.Context == key
			if !match {
				for _, c := range r.Contexts {
					if c == key {
						match = true
						break
					}
				}
			}
			if !match {
				continue
			}
		}
		return r, true
	}
	return nil, false
}

// FindingDigestOf returns the set of finding IDs from a slice; used to
// diff "what's new since last run".
func FindingDigestOf(fs []finding.Finding) map[string]bool {
	out := make(map[string]bool, len(fs))
	for _, f := range fs {
		out[f.ID()] = true
	}
	return out
}

// DiffNew returns the IDs in `current` not present in `previous`.
func DiffNew(current, previous map[string]bool) []string {
	var out []string
	for id := range current {
		if !previous[id] {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

// ---- fleet wave (resumable) ----------------------------------------

// TenantStatus is the per-tenant status in a fleet wave.
type TenantStatus string

const (
	TenantPending      TenantStatus = "pending"
	TenantSnapshotting TenantStatus = "snapshotting"
	TenantUpgrading    TenantStatus = "upgrading"
	TenantDone         TenantStatus = "done"
	TenantFailed       TenantStatus = "failed"
	TenantSkipped      TenantStatus = "skipped"
)

// TenantRow is one entry in a fleet wave.
type TenantRow struct {
	Namespace   string       `json:"namespace"`
	Name        string       `json:"name"`
	Status      TenantStatus `json:"status"`
	StartedAt   *time.Time   `json:"startedAt,omitempty"`
	EndedAt     *time.Time   `json:"endedAt,omitempty"`
	Error       string       `json:"error,omitempty"`
	FromVersion string       `json:"fromVersion,omitempty"`
	ToVersion   string       `json:"toVersion,omitempty"`
}

// Wave is a resumable fleet upgrade run.
type Wave struct {
	ID         string      `json:"id"`
	HostTarget string      `json:"hostTarget,omitempty"`
	VCTarget   string      `json:"vcTarget,omitempty"`
	StartedAt  time.Time   `json:"startedAt"`
	UpdatedAt  time.Time   `json:"updatedAt"`
	Tenants    []TenantRow `json:"tenants"`
}

func wavePath(id string) (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Join(dir, "waves"), 0o755); err != nil {
		return "", err
	}
	return filepath.Join(dir, "waves", id+".json"), nil
}

// SaveWave atomically persists a wave.
func SaveWave(w *Wave) error {
	if w.ID == "" {
		return fmt.Errorf("wave has no ID")
	}
	w.UpdatedAt = time.Now()
	p, err := wavePath(w.ID)
	if err != nil {
		return err
	}
	mu := lockFor(p)
	mu.Lock()
	defer mu.Unlock()
	return writeJSON(p, w)
}

// LoadWave reads an existing wave for --resume.
func LoadWave(id string) (*Wave, error) {
	p, err := wavePath(id)
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	w := &Wave{}
	if err := json.Unmarshal(b, w); err != nil {
		return nil, err
	}
	return w, nil
}

// MostRecentIncompleteWave returns the newest wave that has any
// non-terminal tenant — used by `--resume` with no --wave-id.
func MostRecentIncompleteWave() (*Wave, error) {
	dir, err := Dir()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(filepath.Join(dir, "waves"))
	if err != nil {
		return nil, err
	}
	var best *Wave
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		w, err := LoadWave(strings.TrimSuffix(e.Name(), ".json"))
		if err != nil {
			continue
		}
		if !hasIncomplete(w) {
			continue
		}
		if best == nil || w.UpdatedAt.After(best.UpdatedAt) {
			best = w
		}
	}
	return best, nil
}

func hasIncomplete(w *Wave) bool {
	for _, t := range w.Tenants {
		if t.Status != TenantDone && t.Status != TenantSkipped {
			return true
		}
	}
	return false
}

// ---- helpers --------------------------------------------------------

func writeJSON(p string, v any) error {
	tmp, err := os.CreateTemp(filepath.Dir(p), filepath.Base(p)+".*")
	if err != nil {
		return err
	}
	enc := json.NewEncoder(tmp)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), p)
}

// lockFor returns a process-local mutex per file path. Good enough
// for parallel goroutines within one CLI invocation; we don't try to
// guard against multiple kubectl-upgrade processes concurrently.
var (
	lockMu sync.Mutex
	locks  = map[string]*sync.Mutex{}
)

func lockFor(path string) *sync.Mutex {
	lockMu.Lock()
	defer lockMu.Unlock()
	if m, ok := locks[path]; ok {
		return m
	}
	m := &sync.Mutex{}
	locks[path] = m
	return m
}
