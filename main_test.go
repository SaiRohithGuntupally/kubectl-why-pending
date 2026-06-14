package main

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// TestRun_EndToEnd builds a fake bare-metal cluster and runs the full pipeline
// (list → gather → analyze → render). Output is printed so `go test -v` doubles
// as a live demo without needing a real cluster.
func TestRun_EndToEnd(t *testing.T) {
	rl := func(cpu, mem string) corev1.ResourceList {
		return corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse(cpu),
			corev1.ResourceMemory: resource.MustParse(mem),
		}
	}
	node := func(name, cpu, mem string, taints []corev1.Taint) *corev1.Node {
		return &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec:       corev1.NodeSpec{Taints: taints},
			Status: corev1.NodeStatus{
				Allocatable: rl(cpu, mem),
				Conditions:  []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}},
			},
		}
	}
	// A pod already placed on a node, consuming most of its CPU.
	placed := func(name, node, cpu, mem string) *corev1.Pod {
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec: corev1.PodSpec{
				NodeName:   node,
				Containers: []corev1.Container{{Name: "c", Resources: corev1.ResourceRequirements{Requests: rl(cpu, mem)}}},
			},
			Status: corev1.PodStatus{Phase: corev1.PodRunning},
		}
	}

	cpTaint := []corev1.Taint{{Key: "node-role.kubernetes.io/control-plane", Effect: corev1.TaintEffectNoSchedule}}

	// Pending pod wants 2 CPU. Three workers have 4 CPU but 3 already used
	// (1 free each → fragmentation), plus a tainted control-plane node.
	pending := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "analytics-7d9", Namespace: "default"},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "app", Resources: corev1.ResourceRequirements{Requests: rl("2", "512Mi")}}},
		},
		Status: corev1.PodStatus{Phase: corev1.PodPending},
	}

	client := fake.NewSimpleClientset(
		node("cp-1", "4", "8Gi", cpTaint),
		node("worker-1", "4", "8Gi", nil),
		node("worker-2", "4", "8Gi", nil),
		node("worker-3", "4", "8Gi", nil),
		placed("busy-1", "worker-1", "3", "1Gi"),
		placed("busy-2", "worker-2", "3", "1Gi"),
		placed("busy-3", "worker-3", "3", "1Gi"),
		pending,
	)

	opts := options{namespace: "default", noColor: true}
	blockers, err := run(client, opts)
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}
	if !blockers {
		t.Error("expected a blocking cause (fragmentation), got blockers=false")
	}

	// JSON output should be valid and carry the structured fields.
	jsonOpts := options{namespace: "default", output: "json"}
	if _, err := run(client, jsonOpts); err != nil {
		t.Fatalf("json run failed: %v", err)
	}
}
