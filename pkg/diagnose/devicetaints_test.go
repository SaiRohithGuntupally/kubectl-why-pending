package diagnose

import (
	"strings"
	"testing"

	resourcev1 "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// taintedSlice builds a ResourceSlice whose devices all carry the given inline taints.
func taintedSlice(driver, pool string, taints []resourcev1.DeviceTaint, devices ...string) resourcev1.ResourceSlice {
	s := draSlice(driver, devices...)
	s.Spec.Pool.Name = pool
	for i := range s.Spec.Devices {
		s.Spec.Devices[i].Taints = taints
	}
	return s
}

func taintRule(name string, sel *resourcev1.DeviceTaintSelector, key, value string, effect resourcev1.DeviceTaintEffect) resourcev1.DeviceTaintRule {
	return resourcev1.DeviceTaintRule{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: resourcev1.DeviceTaintRuleSpec{
			DeviceSelector: sel,
			Taint:          resourcev1.DeviceTaint{Key: key, Value: value, Effect: effect},
		},
	}
}

// tolerating returns a copy of the claim whose first request carries the tolerations.
func tolerating(c resourcev1.ResourceClaim, tols ...resourcev1.DeviceToleration) resourcev1.ResourceClaim {
	c.Spec.Devices.Requests[0].Exactly.Tolerations = tols
	return c
}

var (
	unhealthy = resourcev1.DeviceTaint{Key: "gpu.example.com/unhealthy", Value: "true", Effect: resourcev1.DeviceTaintEffectNoSchedule}
	maint     = resourcev1.DeviceTaintEffectNoExecute
)

func TestDeviceTolerationTolerates(t *testing.T) {
	taint := unhealthy
	cases := []struct {
		name string
		tol  resourcev1.DeviceToleration
		want bool
	}{
		{"equal default op", resourcev1.DeviceToleration{Key: taint.Key, Value: "true"}, true},
		{"equal wrong value", resourcev1.DeviceToleration{Key: taint.Key, Value: "false"}, false},
		{"exists any value", resourcev1.DeviceToleration{Key: taint.Key, Operator: resourcev1.DeviceTolerationOpExists}, true},
		{"exists wrong key", resourcev1.DeviceToleration{Key: "other", Operator: resourcev1.DeviceTolerationOpExists}, false},
		{"empty key exists = wildcard", resourcev1.DeviceToleration{Operator: resourcev1.DeviceTolerationOpExists}, true},
		{"empty key equal = nothing", resourcev1.DeviceToleration{}, false},
		{"effect mismatch", resourcev1.DeviceToleration{Key: taint.Key, Value: "true", Effect: resourcev1.DeviceTaintEffectNoExecute}, false},
		{"effect match", resourcev1.DeviceToleration{Key: taint.Key, Value: "true", Effect: resourcev1.DeviceTaintEffectNoSchedule}, true},
	}
	for _, c := range cases {
		if got := deviceTolerationTolerates(c.tol, taint); got != c.want {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}

func TestRuleMatches(t *testing.T) {
	d := publishedDevice{Driver: "gpu.example.com", Pool: "node-3", Name: "gpu-0"}
	sp := func(s string) *string { return &s }
	cases := []struct {
		name string
		sel  *resourcev1.DeviceTaintSelector
		want bool
	}{
		{"nil selector matches nothing", nil, false},
		{"empty selector matches all", &resourcev1.DeviceTaintSelector{}, true},
		{"driver match", &resourcev1.DeviceTaintSelector{Driver: sp("gpu.example.com")}, true},
		{"driver mismatch", &resourcev1.DeviceTaintSelector{Driver: sp("other.example.com")}, false},
		{"driver+pool+device", &resourcev1.DeviceTaintSelector{Driver: sp("gpu.example.com"), Pool: sp("node-3"), Device: sp("gpu-0")}, true},
		{"pool mismatch", &resourcev1.DeviceTaintSelector{Pool: sp("node-4")}, false},
		{"device mismatch", &resourcev1.DeviceTaintSelector{Device: sp("gpu-1")}, false},
	}
	for _, c := range cases {
		if got := ruleMatches(c.sel, d); got != c.want {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}

func TestAnalyzeDRA_DeviceTaintRuleBlocksAllDevices(t *testing.T) {
	pod := draPod("train", "ml", "gpu", "claim-1")
	claims := []resourcev1.ResourceClaim{draClaim("ml", "claim-1", "gpu.example.com", false)}
	classes := []resourcev1.DeviceClass{draClass("gpu.example.com")}
	slices := []resourcev1.ResourceSlice{taintedSlice("gpu.example.com", "node-3", nil, "gpu-0", "gpu-1")}
	drv := "gpu.example.com"
	rules := []resourcev1.DeviceTaintRule{taintRule("drain-node-3", &resourcev1.DeviceTaintSelector{Driver: &drv}, "maintenance", "", maint)}

	c := AnalyzeDRA(pod, claims, slices, classes, rules)
	txt := causeText(c)
	for _, want := range []string{"is not allocated", "All 2 published device(s)", "maintenance:NoExecute", `via DeviceTaintRule "drain-node-3"`, "on 2 device(s)"} {
		if !strings.Contains(txt, want) {
			t.Errorf("missing %q in:\n%s", want, txt)
		}
	}
	if strings.Contains(txt, "likely all already in use") {
		t.Errorf("generic guess should be replaced by the taint diagnosis:\n%s", txt)
	}
	if len(c) != 1 || c[0].Severity != Blocker || !strings.Contains(c[0].Fix, "tolerations") {
		t.Errorf("want one Blocker with a toleration fix, got %+v", c)
	}
}

func TestAnalyzeDRA_InlineDeviceTaintBlocks(t *testing.T) {
	pod := draPod("train", "ml", "gpu", "claim-1")
	claims := []resourcev1.ResourceClaim{draClaim("ml", "claim-1", "gpu.example.com", false)}
	classes := []resourcev1.DeviceClass{draClass("gpu.example.com")}
	slices := []resourcev1.ResourceSlice{taintedSlice("gpu.example.com", "node-3", []resourcev1.DeviceTaint{unhealthy}, "gpu-0")}

	txt := causeText(AnalyzeDRA(pod, claims, slices, classes, nil))
	for _, want := range []string{"All 1 published device(s)", "gpu.example.com/unhealthy=true:NoSchedule", "set by the driver in its ResourceSlice"} {
		if !strings.Contains(txt, want) {
			t.Errorf("missing %q in:\n%s", want, txt)
		}
	}
}

func TestAnalyzeDRA_ClaimToleratesTaint(t *testing.T) {
	pod := draPod("train", "ml", "gpu", "claim-1")
	claim := tolerating(draClaim("ml", "claim-1", "gpu.example.com", false),
		resourcev1.DeviceToleration{Key: unhealthy.Key, Operator: resourcev1.DeviceTolerationOpExists})
	classes := []resourcev1.DeviceClass{draClass("gpu.example.com")}
	slices := []resourcev1.ResourceSlice{taintedSlice("gpu.example.com", "node-3", []resourcev1.DeviceTaint{unhealthy}, "gpu-0")}

	txt := causeText(AnalyzeDRA(pod, []resourcev1.ResourceClaim{claim}, slices, classes, nil))
	if strings.Contains(txt, "taint") {
		t.Errorf("tolerated taint must not be reported as the cause:\n%s", txt)
	}
	if !strings.Contains(txt, "likely all already in use") {
		t.Errorf("want fallback generic detail when taints are tolerated:\n%s", txt)
	}
}

func TestAnalyzeDRA_RuleForOtherDriverIgnored(t *testing.T) {
	pod := draPod("train", "ml", "gpu", "claim-1")
	claims := []resourcev1.ResourceClaim{draClaim("ml", "claim-1", "gpu.example.com", false)}
	classes := []resourcev1.DeviceClass{draClass("gpu.example.com")}
	slices := []resourcev1.ResourceSlice{taintedSlice("gpu.example.com", "node-3", nil, "gpu-0")}
	other := "nic.example.com"
	rules := []resourcev1.DeviceTaintRule{taintRule("drain-nics", &resourcev1.DeviceTaintSelector{Driver: &other}, "maintenance", "", maint)}

	txt := causeText(AnalyzeDRA(pod, claims, slices, classes, rules))
	if strings.Contains(txt, "taint") {
		t.Errorf("rule for another driver must not be reported:\n%s", txt)
	}
}

func TestAnalyzeDRA_NoneEffectIgnored(t *testing.T) {
	pod := draPod("train", "ml", "gpu", "claim-1")
	claims := []resourcev1.ResourceClaim{draClaim("ml", "claim-1", "gpu.example.com", false)}
	classes := []resourcev1.DeviceClass{draClass("gpu.example.com")}
	none := resourcev1.DeviceTaint{Key: "note", Effect: resourcev1.DeviceTaintEffectNone}
	future := resourcev1.DeviceTaint{Key: "note", Effect: "SomethingNew"}
	slices := []resourcev1.ResourceSlice{taintedSlice("gpu.example.com", "node-3", []resourcev1.DeviceTaint{none, future}, "gpu-0")}

	txt := causeText(AnalyzeDRA(pod, claims, slices, classes, nil))
	if strings.Contains(txt, "taint") {
		t.Errorf("None/unknown effects must not block:\n%s", txt)
	}
}

func TestAnalyzeDRA_PartialTaintNamesRemainingCandidates(t *testing.T) {
	pod := draPod("train", "ml", "gpu", "claim-1")
	claims := []resourcev1.ResourceClaim{draClaim("ml", "claim-1", "gpu.example.com", false)}
	classes := []resourcev1.DeviceClass{draClass("gpu.example.com")}
	slices := []resourcev1.ResourceSlice{
		taintedSlice("gpu.example.com", "node-3", []resourcev1.DeviceTaint{unhealthy}, "gpu-0", "gpu-1"),
		taintedSlice("gpu.example.com", "node-4", nil, "gpu-0"),
	}

	txt := causeText(AnalyzeDRA(pod, claims, slices, classes, nil))
	for _, want := range []string{"2 of 3 published device(s) carry device taints", "only 1 device(s) were actually candidates"} {
		if !strings.Contains(txt, want) {
			t.Errorf("missing %q in:\n%s", want, txt)
		}
	}
	if strings.Contains(txt, "All 3") {
		t.Errorf("partial taint must not claim every device is blocked:\n%s", txt)
	}
}

func TestAnalyzeDRA_FirstAvailableTolerationsCount(t *testing.T) {
	pod := draPod("train", "ml", "gpu", "claim-1")
	claim := draClaim("ml", "claim-1", "gpu.example.com", false)
	claim.Spec.Devices.Requests = []resourcev1.DeviceRequest{{
		Name: "gpu",
		FirstAvailable: []resourcev1.DeviceSubRequest{
			{Name: "big", DeviceClassName: "gpu.example.com"},
			{Name: "any", DeviceClassName: "gpu.example.com", Tolerations: []resourcev1.DeviceToleration{{Operator: resourcev1.DeviceTolerationOpExists}}},
		},
	}}
	classes := []resourcev1.DeviceClass{draClass("gpu.example.com")}
	slices := []resourcev1.ResourceSlice{taintedSlice("gpu.example.com", "node-3", []resourcev1.DeviceTaint{unhealthy}, "gpu-0")}

	txt := causeText(AnalyzeDRA(pod, []resourcev1.ResourceClaim{claim}, slices, classes, nil))
	if strings.Contains(txt, "taint") {
		t.Errorf("a tolerating subrequest makes the device a candidate; got:\n%s", txt)
	}
}

func TestAnalyze_DeviceTaintEndToEnd(t *testing.T) {
	pod := draPod("train", "ml", "gpu", "claim-1")
	drv := "gpu.example.com"
	in := Input{
		Pod:           pod,
		Nodes:         []NodeView{{Node: readyNode("n1", "8", "32Gi")}},
		DRAClaims:     []resourcev1.ResourceClaim{draClaim("ml", "claim-1", "gpu.example.com", false)},
		DRAClasses:    []resourcev1.DeviceClass{draClass("gpu.example.com")},
		DRASlices:     []resourcev1.ResourceSlice{taintedSlice("gpu.example.com", "n1", nil, "gpu-0")},
		DRATaintRules: []resourcev1.DeviceTaintRule{taintRule("drain", &resourcev1.DeviceTaintSelector{Driver: &drv}, "maintenance", "", maint)},
	}
	r := Analyze(in)
	if !r.HasBlocker() || !hasCause(r, "is not allocated") {
		t.Fatalf("want DRA blocker surfaced through Analyze, got:\n%s", titles(r))
	}
	found := false
	for _, c := range r.Causes {
		if strings.Contains(c.Detail, `DeviceTaintRule "drain"`) {
			found = true
		}
	}
	if !found {
		t.Fatalf("want DeviceTaintRule named in detail, got:\n%s", titles(r))
	}
}
