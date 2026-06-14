package diagnose

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestResult_NodeVerdicts(t *testing.T) {
	// One cordoned node, one that fits — both should appear with verdicts.
	p := pod("100m", "64Mi")
	cordoned := readyNode("n1", "4", "8Gi")
	cordoned.Spec.Unschedulable = true
	r := Analyze(Input{Pod: p, Nodes: []NodeView{
		{Node: cordoned},
		{Node: readyNode("n2", "4", "8Gi")},
	}})

	verdicts := map[string]NodeVerdict{}
	for _, n := range r.Nodes {
		verdicts[n.Name] = n
	}
	if len(verdicts) != 2 {
		t.Fatalf("want 2 node verdicts, got %d: %+v", len(verdicts), r.Nodes)
	}
	if verdicts["n1"].Schedulable || !strings.Contains(verdicts["n1"].Reason, "cordoned") {
		t.Errorf("n1 verdict wrong: %+v", verdicts["n1"])
	}
	if !verdicts["n2"].Schedulable {
		t.Errorf("n2 should be schedulable: %+v", verdicts["n2"])
	}
}

func TestResult_JSONShape(t *testing.T) {
	p := gpuPod(1)
	r := Analyze(Input{Pod: p, Nodes: []NodeView{{Node: readyNode("n1", "8", "16Gi")}}})

	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)

	// Severity must serialize as a name, not an integer.
	if !strings.Contains(s, `"severity":"blocker"`) {
		t.Errorf("expected severity as string in JSON, got: %s", s)
	}
	// Stable lower-camel keys for consumers.
	for _, key := range []string{`"pod":`, `"request":`, `"causes":`, `"nodes":`, `"cpuMilli":`} {
		if !strings.Contains(s, key) {
			t.Errorf("missing key %s in JSON: %s", key, s)
		}
	}

	// Round-trips back into a generic structure.
	var back map[string]any
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("json not parseable: %v", err)
	}
}

func TestResult_HasBlocker(t *testing.T) {
	// A pod that fits everywhere has no blocker (only the info fallback).
	p := pod("100m", "64Mi")
	r := Analyze(Input{Pod: p, Nodes: []NodeView{{Node: readyNode("n1", "8", "16Gi")}}})
	if r.HasBlocker() {
		t.Errorf("did not expect a blocker, got: %+v", r.Causes)
	}

	// A GPU pod with no GPU node has a blocker.
	g := Analyze(Input{Pod: gpuPod(1), Nodes: []NodeView{{Node: readyNode("n1", "8", "16Gi")}}})
	if !g.HasBlocker() {
		t.Errorf("expected a blocker for the GPU pod")
	}
}
