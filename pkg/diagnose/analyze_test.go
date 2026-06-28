package diagnose

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func cpuMem(cpu, mem string) corev1.ResourceList {
	return corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse(cpu),
		corev1.ResourceMemory: resource.MustParse(mem),
	}
}

func readyNode(name, cpu, mem string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{"kubernetes.io/hostname": name}},
		Status: corev1.NodeStatus{
			Allocatable: cpuMem(cpu, mem),
			Conditions:  []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}},
		},
	}
}

func pod(cpu, mem string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "demo", Namespace: "default"},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{
				Name:      "app",
				Resources: corev1.ResourceRequirements{Requests: cpuMem(cpu, mem)},
			}},
		},
	}
}

func titles(r Result) string {
	var b strings.Builder
	for _, c := range r.Causes {
		b.WriteString(c.Title)
		b.WriteString("\n")
	}
	return b.String()
}

func hasCause(r Result, substr string) bool {
	return strings.Contains(titles(r), substr)
}

func TestPodRequests_SumAndInitMax(t *testing.T) {
	p := pod("500m", "256Mi")
	p.Spec.Containers = append(p.Spec.Containers, corev1.Container{
		Name: "sidecar", Resources: corev1.ResourceRequirements{Requests: cpuMem("250m", "128Mi")},
	})
	p.Spec.InitContainers = []corev1.Container{{
		Name: "init", Resources: corev1.ResourceRequirements{Requests: cpuMem("1", "1Gi")},
	}}
	got := PodRequests(p)
	// sum = 750m / 384Mi, init = 1000m / 1Gi -> effective = max per dimension.
	if got.CPUMilli != 1000 {
		t.Errorf("CPU: want 1000m, got %dm", got.CPUMilli)
	}
	if got.MemBytes != 1024*1024*1024 {
		t.Errorf("Mem: want 1Gi, got %s", FormatMem(got.MemBytes))
	}
}

func TestFragmentation(t *testing.T) {
	// Pod wants 2 CPU. Three nodes with 4 CPU each but 3 CPU already used ->
	// 1 CPU free each: 3 CPU free in total, but no single node fits 2.
	p := pod("2", "256Mi")
	nodes := []NodeView{
		{Node: readyNode("n1", "4", "8Gi"), Used: Resources{CPUMilli: 3000, MemBytes: 1 << 30}},
		{Node: readyNode("n2", "4", "8Gi"), Used: Resources{CPUMilli: 3000, MemBytes: 1 << 30}},
		{Node: readyNode("n3", "4", "8Gi"), Used: Resources{CPUMilli: 3000, MemBytes: 1 << 30}},
	}
	r := Analyze(Input{Pod: p, Nodes: nodes})
	if !hasCause(r, "fragmentation") {
		t.Fatalf("expected fragmentation cause, got:\n%s", titles(r))
	}
}

func TestPodLargerThanAnyNode(t *testing.T) {
	// Pod wants 8 CPU; biggest node is 4 CPU even when empty. Defrag can't help.
	p := pod("8", "256Mi")
	nodes := []NodeView{
		{Node: readyNode("n1", "4", "8Gi")},
		{Node: readyNode("n2", "4", "8Gi")},
	}
	r := Analyze(Input{Pod: p, Nodes: nodes})
	if !hasCause(r, "larger than any single node") {
		t.Fatalf("expected pod-too-large cause, got:\n%s", titles(r))
	}
}

func TestInsufficientFreeCapacity(t *testing.T) {
	// Pod wants 3 CPU; nodes are 4 CPU but nearly full (0.5 free each), and the
	// aggregate free (1 CPU) is still under the request. Not fragmentation.
	p := pod("3", "256Mi")
	nodes := []NodeView{
		{Node: readyNode("n1", "4", "8Gi"), Used: Resources{CPUMilli: 3500, MemBytes: 1 << 30}},
		{Node: readyNode("n2", "4", "8Gi"), Used: Resources{CPUMilli: 3500, MemBytes: 1 << 30}},
	}
	r := Analyze(Input{Pod: p, Nodes: nodes})
	if !hasCause(r, "Insufficient free capacity") {
		t.Fatalf("expected insufficient free capacity, got:\n%s", titles(r))
	}
}

func TestControlPlaneTaintBlocks(t *testing.T) {
	p := pod("100m", "64Mi")
	n := readyNode("cp", "4", "8Gi")
	n.Spec.Taints = []corev1.Taint{{
		Key: "node-role.kubernetes.io/control-plane", Effect: corev1.TaintEffectNoSchedule,
	}}
	r := Analyze(Input{Pod: p, Nodes: []NodeView{{Node: n}}})
	if !hasCause(r, "taints the pod doesn't tolerate") {
		t.Fatalf("expected taint cause, got:\n%s", titles(r))
	}
	// Single tainted node => the taint is the blocker.
	if r.Causes[0].Severity != Blocker {
		t.Errorf("expected taint to be a Blocker, got severity %d", r.Causes[0].Severity)
	}
}

func TestTolerationLetsItSchedule(t *testing.T) {
	p := pod("100m", "64Mi")
	p.Spec.Tolerations = []corev1.Toleration{{
		Key: "node-role.kubernetes.io/control-plane", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule,
	}}
	n := readyNode("cp", "4", "8Gi")
	n.Spec.Taints = []corev1.Taint{{Key: "node-role.kubernetes.io/control-plane", Effect: corev1.TaintEffectNoSchedule}}
	r := Analyze(Input{Pod: p, Nodes: []NodeView{{Node: n}}})
	if hasCause(r, "taint") {
		t.Fatalf("toleration should clear the taint, got:\n%s", titles(r))
	}
}

func TestNodeSelectorMismatch(t *testing.T) {
	p := pod("100m", "64Mi")
	p.Spec.NodeSelector = map[string]string{"disktype": "ssd"}
	r := Analyze(Input{Pod: p, Nodes: []NodeView{{Node: readyNode("n1", "4", "8Gi")}}})
	if !hasCause(r, "nodeSelector") {
		t.Fatalf("expected nodeSelector cause, got:\n%s", titles(r))
	}
}

func TestMissingPVC(t *testing.T) {
	p := pod("100m", "64Mi")
	r := Analyze(Input{
		Pod:         p,
		Nodes:       []NodeView{{Node: readyNode("n1", "4", "8Gi")}},
		MissingPVCs: []string{"data-demo-0"},
	})
	if !hasCause(r, "PersistentVolumeClaim(s) not found") {
		t.Fatalf("expected missing-PVC cause, got:\n%s", titles(r))
	}
}

func TestUnboundPVC(t *testing.T) {
	p := pod("100m", "64Mi")
	r := Analyze(Input{
		Pod:         p,
		Nodes:       []NodeView{{Node: readyNode("n1", "4", "8Gi")}},
		UnboundPVCs: []string{"data-demo-0"},
	})
	if !hasCause(r, "Unbound PersistentVolumeClaim") {
		t.Fatalf("expected PVC cause, got:\n%s", titles(r))
	}
}

func TestHealthyFitFallsBackToDynamic(t *testing.T) {
	p := pod("100m", "64Mi")
	r := Analyze(Input{Pod: p, Nodes: []NodeView{{Node: readyNode("n1", "4", "8Gi")}}})
	if !hasCause(r, "should fit") {
		t.Fatalf("expected dynamic-cause fallback, got:\n%s", titles(r))
	}
}

// notReadyNode returns a node that has capacity but reports NotReady.
func notReadyNode(name, cpu, mem string) *corev1.Node {
	n := readyNode(name, cpu, mem)
	n.Status.Conditions = []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionFalse}}
	return n
}

func TestAllNodesNotReadyIsABlocker(t *testing.T) {
	// Every node is NotReady => there is nowhere to schedule, so the diagnosis
	// must be a Blocker (and HasBlocker must be true for the exit code), not a
	// soft warning that exits 0.
	p := pod("100m", "64Mi")
	r := Analyze(Input{Pod: p, Nodes: []NodeView{
		{Node: notReadyNode("n1", "4", "8Gi")},
		{Node: notReadyNode("n2", "4", "8Gi")},
	}})
	if !hasCause(r, "NotReady") {
		t.Fatalf("expected NotReady cause, got:\n%s", titles(r))
	}
	if !r.HasBlocker() {
		t.Fatalf("all-NotReady cluster must be a Blocker, got:\n%s", titles(r))
	}
}

func TestSomeNodesNotReadyStaysWarning(t *testing.T) {
	// One NotReady node, but a healthy node can host the pod => NotReady is only
	// a contributing warning, not the blocker.
	p := pod("100m", "64Mi")
	r := Analyze(Input{Pod: p, Nodes: []NodeView{
		{Node: notReadyNode("n1", "4", "8Gi")},
		{Node: readyNode("n2", "4", "8Gi")},
	}})
	if r.HasBlocker() {
		t.Fatalf("a healthy node exists; NotReady should not be a blocker, got:\n%s", titles(r))
	}
}

func TestNeverEmptyDiagnosis(t *testing.T) {
	// Regression guard: Analyze must always return at least one cause so the
	// report never prints an empty diagnosis.
	cases := map[string]Input{
		"healthy fit": {Pod: pod("100m", "64Mi"), Nodes: []NodeView{{Node: readyNode("n1", "4", "8Gi")}}},
		"no nodes":    {Pod: pod("100m", "64Mi")},
		"all notready": {Pod: pod("100m", "64Mi"), Nodes: []NodeView{
			{Node: notReadyNode("n1", "4", "8Gi")},
		}},
	}
	for name, in := range cases {
		if r := Analyze(in); len(r.Causes) == 0 {
			t.Errorf("%s: expected a non-empty diagnosis, got zero causes", name)
		}
	}
}
