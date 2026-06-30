package diagnose

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	resourcev1 "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func sptr(s string) *string { return &s }

func draPod(name, ns, podClaim, claimName string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: corev1.PodSpec{
			ResourceClaims: []corev1.PodResourceClaim{{Name: podClaim, ResourceClaimName: sptr(claimName)}},
		},
	}
}

func draClaim(ns, name, class string, allocated bool) resourcev1.ResourceClaim {
	c := resourcev1.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: resourcev1.ResourceClaimSpec{
			Devices: resourcev1.DeviceClaim{
				Requests: []resourcev1.DeviceRequest{
					{Name: "gpu", Exactly: &resourcev1.ExactDeviceRequest{DeviceClassName: class, Count: 1}},
				},
			},
		},
	}
	if allocated {
		c.Status.Allocation = &resourcev1.AllocationResult{}
	}
	return c
}

func draClass(name string) resourcev1.DeviceClass {
	return resourcev1.DeviceClass{ObjectMeta: metav1.ObjectMeta{Name: name}}
}

func draSlice(driver string, devices ...string) resourcev1.ResourceSlice {
	var ds []resourcev1.Device
	for _, d := range devices {
		ds = append(ds, resourcev1.Device{Name: d})
	}
	return resourcev1.ResourceSlice{Spec: resourcev1.ResourceSliceSpec{Driver: driver, Devices: ds}}
}

func causeText(causes []Cause) string {
	var b strings.Builder
	for _, c := range causes {
		b.WriteString(c.Title + " :: " + c.Detail + "\n")
	}
	return b.String()
}

func TestUsesDRA(t *testing.T) {
	if UsesDRA(&corev1.Pod{}) {
		t.Error("plain pod should not use DRA")
	}
	if !UsesDRA(draPod("p", "ml", "gpu", "claim-1")) {
		t.Error("pod with resourceClaims should use DRA")
	}
}

func TestAnalyzeDRA_Unallocated(t *testing.T) {
	pod := draPod("train", "ml", "gpu", "claim-1")
	claims := []resourcev1.ResourceClaim{draClaim("ml", "claim-1", "gpu.example.com", false)}
	c := AnalyzeDRA(pod, claims, nil, nil)
	if !strings.Contains(causeText(c), "is not allocated") {
		t.Fatalf("want unallocated cause, got:\n%s", causeText(c))
	}
}

func TestAnalyzeDRA_Allocated_NoCause(t *testing.T) {
	pod := draPod("train", "ml", "gpu", "claim-1")
	claims := []resourcev1.ResourceClaim{draClaim("ml", "claim-1", "gpu.example.com", true)}
	if c := AnalyzeDRA(pod, claims, nil, nil); len(c) != 0 {
		t.Fatalf("allocated claim should produce no cause, got:\n%s", causeText(c))
	}
}

func TestAnalyzeDRA_ClaimNotFound(t *testing.T) {
	pod := draPod("train", "ml", "gpu", "claim-1")
	if c := AnalyzeDRA(pod, nil, nil, nil); !strings.Contains(causeText(c), "not found") {
		t.Fatalf("want claim-not-found, got:\n%s", causeText(c))
	}
}

func TestAnalyzeDRA_NamespaceIsolation(t *testing.T) {
	// A claim with the right name but in another namespace must not match.
	pod := draPod("train", "ml", "gpu", "claim-1")
	claims := []resourcev1.ResourceClaim{draClaim("other", "claim-1", "gpu.example.com", true)}
	if c := AnalyzeDRA(pod, claims, nil, nil); !strings.Contains(causeText(c), "not found") {
		t.Fatalf("claim in another namespace must not match, got:\n%s", causeText(c))
	}
}

func TestAnalyzeDRA_MissingDeviceClass(t *testing.T) {
	pod := draPod("train", "ml", "gpu", "claim-1")
	claims := []resourcev1.ResourceClaim{draClaim("ml", "claim-1", "gpu.example.com", false)}
	classes := []resourcev1.DeviceClass{draClass("some.other.class")}
	c := AnalyzeDRA(pod, claims, []resourcev1.ResourceSlice{}, classes)
	if !strings.Contains(causeText(c), "not found in the cluster") {
		t.Fatalf("want missing-DeviceClass detail, got:\n%s", causeText(c))
	}
}

func TestAnalyzeDRA_NoDriverPublishing(t *testing.T) {
	pod := draPod("train", "ml", "gpu", "claim-1")
	claims := []resourcev1.ResourceClaim{draClaim("ml", "claim-1", "gpu.example.com", false)}
	classes := []resourcev1.DeviceClass{draClass("gpu.example.com")}
	c := AnalyzeDRA(pod, claims, []resourcev1.ResourceSlice{}, classes)
	if !strings.Contains(causeText(c), "No ResourceSlices publish any devices") {
		t.Fatalf("want no-driver detail, got:\n%s", causeText(c))
	}
}

func TestAnalyzeDRA_TemplateNotMaterialized(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "train", Namespace: "ml"},
		Spec: corev1.PodSpec{
			ResourceClaims: []corev1.PodResourceClaim{{Name: "gpu", ResourceClaimTemplateName: sptr("gpu-template")}},
		},
	}
	if c := AnalyzeDRA(pod, nil, nil, nil); !strings.Contains(causeText(c), "not created yet") {
		t.Fatalf("want template-not-materialized cause, got:\n%s", causeText(c))
	}
}

func TestAnalyze_DRAUnallocatedEndToEnd(t *testing.T) {
	pod := draPod("train", "ml", "gpu", "claim-1")
	in := Input{
		Pod:        pod,
		Nodes:      []NodeView{{Node: readyNode("n1", "8", "32Gi")}},
		DRAClaims:  []resourcev1.ResourceClaim{draClaim("ml", "claim-1", "gpu.example.com", false)},
		DRAClasses: []resourcev1.DeviceClass{draClass("gpu.example.com")},
		DRASlices:  []resourcev1.ResourceSlice{draSlice("gpu.example.com", "d0")},
	}
	r := Analyze(in)
	if !r.HasBlocker() || !hasCause(r, "is not allocated") {
		t.Fatalf("want DRA blocker surfaced through Analyze, got:\n%s", titles(r))
	}
}
