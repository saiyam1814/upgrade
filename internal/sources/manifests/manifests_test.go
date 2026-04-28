package manifests

import "testing"

func TestParseString_MultiDoc(t *testing.T) {
	in := `apiVersion: batch/v1beta1
kind: CronJob
metadata:
  name: a
  namespace: ops
---
apiVersion: policy/v1beta1
kind: PodDisruptionBudget
metadata:
  name: b
  namespace: ops
`
	objs, err := ParseString(in, "x.yaml")
	if err != nil {
		t.Fatalf("ParseString: %v", err)
	}
	if len(objs) != 2 {
		t.Fatalf("want 2 objects, got %d", len(objs))
	}
	if objs[0].Obj.APIVersion != "batch/v1beta1" || objs[0].Obj.Kind != "CronJob" {
		t.Errorf("doc1 wrong: %+v", objs[0])
	}
	if objs[1].Obj.APIVersion != "policy/v1beta1" || objs[1].Obj.Kind != "PodDisruptionBudget" {
		t.Errorf("doc2 wrong: %+v", objs[1])
	}
}

func TestParseString_SkipsNonK8s(t *testing.T) {
	in := `# Helm Chart.yaml
apiVersion: v2
name: my-chart
description: not a kubernetes object
version: 0.1.0
`
	objs, err := ParseString(in, "Chart.yaml")
	if err != nil {
		t.Fatal(err)
	}
	// "v2 / nil-Kind" should be skipped because Kind is empty.
	for _, o := range objs {
		if o.Obj.Kind == "" {
			t.Fatalf("kindless objects should be filtered; got %+v", o)
		}
	}
}
