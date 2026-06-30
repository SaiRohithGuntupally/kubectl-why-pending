package diagnose

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func chainPod(name string, ready bool, waiting string) corev1.Pod {
	cs := corev1.ContainerStatus{Name: "c", Ready: ready}
	if waiting != "" {
		cs.State.Waiting = &corev1.ContainerStateWaiting{Reason: waiting}
		cs.Ready = false
	}
	return corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "gpu-operator"},
		Spec:       corev1.PodSpec{NodeName: "gpu-1"},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{cs}},
	}
}

func TestAnalyzeOperatorChain_FirstBrokenInOrder(t *testing.T) {
	cs := AnalyzeOperatorChain([]corev1.Pod{
		chainPod("nvidia-driver-daemonset-x", false, "CrashLoopBackOff"),
		chainPod("nvidia-device-plugin-daemonset-y", false, "CreateContainerError"),
	})
	if cs.FirstBroken == nil || cs.FirstBroken.Name != "driver" {
		t.Fatalf("driver is earlier in the chain; want it as first broken, got %+v", cs.FirstBroken)
	}
}

func TestAnalyzeOperatorChain_NotDetected(t *testing.T) {
	cs := AnalyzeOperatorChain([]corev1.Pod{
		{ObjectMeta: metav1.ObjectMeta{Name: "some-app"}},
	})
	if cs.Detected {
		t.Fatalf("unrelated pods should not count as a GPU stack: %+v", cs)
	}
}

func TestExtendedNotAdvertised_NamesBrokenChainForGPU(t *testing.T) {
	chain := AnalyzeOperatorChain([]corev1.Pod{
		chainPod("nvidia-device-plugin-daemonset-y", false, "CrashLoopBackOff"),
	})
	c := extendedNotAdvertised("nvidia.com/gpu", 1, &chain)
	if !strings.Contains(c.Title, "device-plugin") || !strings.Contains(c.Fix, "nvidia-device-plugin-daemonset-y") {
		t.Fatalf("want broken device-plugin named in title+fix, got title=%q fix=%q", c.Title, c.Fix)
	}
}

func TestExtendedNotAdvertised_NonGPUStaysGeneric(t *testing.T) {
	chain := AnalyzeOperatorChain(nil) // not detected
	c := extendedNotAdvertised("example.com/fpga", 1, &chain)
	if !strings.Contains(c.Title, "No eligible node provides") {
		t.Fatalf("non-GPU extended resource must use the generic cause, got %q", c.Title)
	}
}

func TestExtendedNotAdvertised_GPUNoStackFound(t *testing.T) {
	chain := AnalyzeOperatorChain(nil) // no GPU stack pods anywhere
	c := extendedNotAdvertised("nvidia.com/gpu", 1, &chain)
	if !strings.Contains(c.Title, "No GPU device plugin found") {
		t.Fatalf("want no-device-plugin cause for GPU, got %q", c.Title)
	}
}

func TestAnalyze_GPUChainEnrichmentEndToEnd(t *testing.T) {
	// GPU pod, a healthy node that has no GPU, and a broken device-plugin: the
	// extended-resource finding should name the broken chain link.
	chain := AnalyzeOperatorChain([]corev1.Pod{
		chainPod("nvidia-device-plugin-daemonset-y", false, "CrashLoopBackOff"),
	})
	in := Input{
		Pod:   gpuPod(1),
		Nodes: []NodeView{{Node: readyNode("n1", "8", "32Gi")}},
		Chain: &chain,
	}
	r := Analyze(in)
	if !hasCause(r, "device-plugin") {
		t.Fatalf("want device-plugin named in the GPU finding, got:\n%s", titles(r))
	}
}
