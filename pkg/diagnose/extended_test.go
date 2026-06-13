package diagnose

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

const gpuName = "nvidia.com/gpu"

func gpuNode(name string, gpus int64) *corev1.Node {
	n := readyNode(name, "8", "32Gi")
	n.Status.Allocatable[corev1.ResourceName(gpuName)] = *resource.NewQuantity(gpus, resource.DecimalSI)
	return n
}

func gpuPod(gpus int64) *corev1.Pod {
	p := pod("500m", "256Mi")
	p.Spec.Containers[0].Resources.Requests[corev1.ResourceName(gpuName)] = *resource.NewQuantity(gpus, resource.DecimalSI)
	return p
}

func TestPodRequests_Extended(t *testing.T) {
	p := gpuPod(2)
	got := PodRequests(p)
	if got.Extended[gpuName] != 2 {
		t.Fatalf("want 2 gpu, got %d", got.Extended[gpuName])
	}
	// init container asking for more GPU sets the effective request to the max.
	p.Spec.InitContainers = []corev1.Container{{
		Name: "warmup",
		Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{
			corev1.ResourceName(gpuName): *resource.NewQuantity(4, resource.DecimalSI),
		}},
	}}
	if got := PodRequests(p); got.Extended[gpuName] != 4 {
		t.Fatalf("want 4 (init max), got %d", got.Extended[gpuName])
	}
}

func TestExtended_NoNodeProvides(t *testing.T) {
	p := gpuPod(1)
	// Eligible nodes exist but none advertises a GPU.
	eligible := []NodeView{{Node: readyNode("n1", "8", "32Gi")}}
	causes := AnalyzeExtendedResources(p, eligible)
	if len(causes) != 1 || causes[0].Title != "No eligible node provides nvidia.com/gpu" {
		t.Fatalf("expected a 'no node provides' cause, got %+v", causes)
	}
}

func TestExtended_Insufficient(t *testing.T) {
	// Two GPU nodes, both fully consumed.
	p := gpuPod(1)
	eligible := []NodeView{
		{Node: gpuNode("g1", 2), Used: Resources{Extended: map[string]int64{gpuName: 2}}},
		{Node: gpuNode("g2", 2), Used: Resources{Extended: map[string]int64{gpuName: 2}}},
	}
	causes := AnalyzeExtendedResources(p, eligible)
	if len(causes) == 0 || causes[0].Title != "Insufficient nvidia.com/gpu" {
		t.Fatalf("expected insufficient-gpu cause, got %+v", causes)
	}
}

func TestExtended_Fits(t *testing.T) {
	// A node with a free GPU — no extended-resource cause.
	p := gpuPod(1)
	eligible := []NodeView{{Node: gpuNode("g1", 4), Used: Resources{Extended: map[string]int64{gpuName: 1}}}}
	if c := AnalyzeExtendedResources(p, eligible); len(c) != 0 {
		t.Fatalf("expected no cause, got %+v", c)
	}
}

func TestExtended_EndToEndThroughAnalyze(t *testing.T) {
	// Pod fits CPU/mem on the node but needs a GPU the node doesn't have.
	p := gpuPod(1)
	r := Analyze(Input{Pod: p, Nodes: []NodeView{{Node: readyNode("n1", "8", "32Gi")}}})
	if !hasCause(r, "No eligible node provides nvidia.com/gpu") {
		t.Fatalf("expected GPU blocker via Analyze, got:\n%s", titles(r))
	}
}
