package apis

import "testing"

func TestParse(t *testing.T) {
	tests := []struct {
		in    string
		major int
		minor int
		ok    bool
	}{
		{"v1.30", 1, 30, true},
		{"1.30", 1, 30, true},
		{"v1.30.0", 1, 30, true},
		{"v1.32.0-rc.1", 1, 32, true},
		{"v1.34+gke.123", 1, 34, true},
		{"", 0, 0, false},
		{"v1", 0, 0, false},
		{"v1.x", 0, 0, false},
	}
	for _, tt := range tests {
		got, ok := Parse(tt.in)
		if ok != tt.ok {
			t.Errorf("Parse(%q): ok=%v want %v", tt.in, ok, tt.ok)
			continue
		}
		if !ok {
			continue
		}
		if got.Major != tt.major || got.Minor != tt.minor {
			t.Errorf("Parse(%q): got %d.%d want %d.%d", tt.in, got.Major, got.Minor, tt.major, tt.minor)
		}
	}
}

func TestLess(t *testing.T) {
	tests := []struct {
		a, b string
		less bool
	}{
		{"v1.30", "v1.31", true},
		{"v1.31", "v1.30", false},
		{"v1.31", "v1.31", false},
		{"v1.31.0", "v1.31.99", false}, // patch ignored
		{"v0.33", "v1.0", true},
	}
	for _, tt := range tests {
		a := MustParse(tt.a)
		b := MustParse(tt.b)
		if got := a.Less(b); got != tt.less {
			t.Errorf("%s.Less(%s) = %v want %v", tt.a, tt.b, got, tt.less)
		}
	}
}

func TestEngineLookup(t *testing.T) {
	e, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	rules := e.Lookup("batch/v1beta1", "CronJob")
	if len(rules) == 0 {
		t.Fatalf("expected at least one rule for batch/v1beta1 CronJob")
	}
	found := false
	for _, r := range rules {
		if r.RemovedIn == "v1.25.0" && r.ReplacementAPI == "batch/v1" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected batch/v1beta1 CronJob → batch/v1 removed in v1.25.0; got %+v", rules)
	}
}
