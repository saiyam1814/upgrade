package finding

import (
	"testing"
)

func mk(sev Severity, cat Category, title string) Finding {
	return Finding{Severity: sev, Category: cat, Title: title}
}

func TestSeverityRank(t *testing.T) {
	if Blocker.Rank() >= High.Rank() {
		t.Errorf("Blocker should outrank High")
	}
	if High.Rank() >= Medium.Rank() {
		t.Errorf("High should outrank Medium")
	}
}

func TestSort_BySeverityThenCategoryThenTitle(t *testing.T) {
	fs := []Finding{
		mk(Medium, CategoryAPI, "Z"),
		mk(Blocker, CategoryAPI, "A"),
		mk(High, CategoryAPI, "M"),
		mk(Blocker, CategoryAPI, "0"),
	}
	Sort(fs)
	if fs[0].Severity != Blocker || fs[0].Title != "0" {
		t.Errorf("first should be BLOCKER 0, got %+v", fs[0])
	}
	if fs[1].Severity != Blocker || fs[1].Title != "A" {
		t.Errorf("second should be BLOCKER A, got %+v", fs[1])
	}
	if fs[3].Severity != Medium {
		t.Errorf("last should be MEDIUM, got %+v", fs[3])
	}
}

func TestDedupe_PrefersOwnerInfo(t *testing.T) {
	a := mk(Blocker, CategoryAPI, "X")
	a.Object = &Object{APIVersion: "v1", Kind: "Pod", Namespace: "ns", Name: "n"}
	b := a
	b.Owner = &Owner{Kind: "Deployment", Namespace: "ns", Name: "ctrl"}

	out := Dedupe([]Finding{a, b})
	if len(out) != 1 {
		t.Fatalf("Dedupe should collapse to 1; got %d", len(out))
	}
	if out[0].Owner == nil {
		t.Error("Dedupe should pick up Owner from second copy")
	}
}

func TestCounts(t *testing.T) {
	fs := []Finding{mk(Blocker, "", ""), mk(Blocker, "", ""), mk(High, "", ""), mk(Medium, "", "")}
	c := Counts(fs)
	if c[Blocker] != 2 || c[High] != 1 || c[Medium] != 1 {
		t.Errorf("Counts wrong: %+v", c)
	}
}

func TestObjectString(t *testing.T) {
	o := Object{APIVersion: "v1", Kind: "Pod", Namespace: "ns", Name: "n"}
	got := o.String()
	want := "v1 Pod ns/n"
	if got != want {
		t.Errorf("Object.String: got %q want %q", got, want)
	}
	o2 := Object{APIVersion: "v1", Kind: "Node", Name: "node-1"}
	if got := o2.String(); got != "v1 Node node-1" {
		t.Errorf("non-namespaced Object.String: got %q", got)
	}
	if (Object{}).String() != "" {
		t.Errorf("zero Object should stringify to empty")
	}
}

func TestID_StableForSameObject(t *testing.T) {
	a := mk(Blocker, CategoryAPI, "X")
	a.Object = &Object{APIVersion: "v1", Kind: "Pod", Namespace: "ns", Name: "n"}
	b := a
	if a.ID() != b.ID() {
		t.Errorf("ID should be stable for same data")
	}
	c := a
	c.Object = &Object{APIVersion: "v1", Kind: "Pod", Namespace: "ns", Name: "other"}
	if a.ID() == c.ID() {
		t.Errorf("ID should differ for different objects")
	}
}
