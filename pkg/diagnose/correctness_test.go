package diagnose

// Regression tests for the "confident wrong answer" cases from the 2026-09
// audit (docs/research/audit-2026-09.md). Each test names its audit risk.

import (
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	resourcev1 "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func severities(r Result) map[Severity]int {
	m := map[Severity]int{}
	for _, c := range r.Causes {
		m[c.Severity]++
	}
	return m
}

// Risk 1: a gated pod gets no capacity analysis and no blocker.
func TestGatedPodIsNotDiagnosedAsCapacity(t *testing.T) {
	p := pod("200", "1Gi") // would be "larger than any node"
	p.Spec.SchedulingGates = []corev1.PodSchedulingGate{{Name: "kueue.x-k8s.io/admission"}}
	p.ManagedFields = []metav1.ManagedFieldsEntry{{
		Manager:  "kueue",
		FieldsV1: &metav1.FieldsV1{Raw: []byte(`{"f:spec":{"f:schedulingGates":{}}}`)},
	}}
	r := Analyze(Input{Pod: p, Nodes: []NodeView{{Node: readyNode("n1", "4", "8Gi")}}})
	if r.HasBlocker() {
		t.Fatalf("gated pod must not have a blocker, got: %s", titles(r))
	}
	if len(r.Causes) != 1 || !strings.Contains(r.Causes[0].Title, "scheduling gate") {
		t.Fatalf("want only the gate cause, got: %s", titles(r))
	}
	if !strings.Contains(r.Causes[0].Detail, "kueue.x-k8s.io/admission") || !strings.Contains(r.Causes[0].Detail, "kueue") {
		t.Errorf("gate name and manager should be named: %s", r.Causes[0].Detail)
	}
}

// Risk 20: a Pending pod already bound to a node is a kubelet problem.
func TestBoundPendingPodPointsAtKubelet(t *testing.T) {
	p := pod("100m", "64Mi")
	p.Spec.NodeName = "worker-1"
	p.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name:  "app",
		State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff", Message: "Back-off pulling image"}},
	}}
	r := Analyze(Input{Pod: p, Nodes: []NodeView{{Node: readyNode("worker-1", "4", "8Gi")}}})
	if r.HasBlocker() {
		t.Fatalf("bound pod must not have a scheduling blocker: %s", titles(r))
	}
	if !hasCause(r, "Already scheduled to worker-1") || !strings.Contains(r.Causes[0].Detail, "ImagePullBackOff") {
		t.Fatalf("want kubelet-side explanation with the waiting reason, got: %s / %s", titles(r), r.Causes[0].Detail)
	}
}

// Risk 2: nominatedNodeName means the scheduler is already acting.
func TestNominatedPodHasNoBlocker(t *testing.T) {
	p := pod("3", "1Gi")
	p.Status.NominatedNodeName = "n1"
	n := readyNode("n1", "4", "8Gi")
	r := Analyze(Input{Pod: p, Nodes: []NodeView{{Node: n, Used: Resources{CPUMilli: 3000}}}})
	if r.HasBlocker() {
		t.Fatalf("nominated pod should be downgraded, got: %s", titles(r))
	}
	if !hasCause(r, "nominated node n1") || !hasCause(r, "Insufficient free capacity") {
		t.Fatalf("want nomination info plus the (downgraded) capacity finding: %s", titles(r))
	}
}

// Risk 3: required affinity that only matches the pod itself is allowed.
func TestSelfMatchingPodAffinityIsAllowed(t *testing.T) {
	p := appPod("web")
	p.Spec.Affinity = &corev1.Affinity{PodAffinity: &corev1.PodAffinity{
		RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{{
			TopologyKey:   "kubernetes.io/hostname",
			LabelSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "web"}},
		}},
	}}
	r := Analyze(Input{Pod: p, Nodes: []NodeView{{Node: readyNode("n1", "4", "8Gi")}}})
	if hasCause(r, "pod affinity") {
		t.Fatalf("first replica of a self-affine workload must schedule, got: %s", titles(r))
	}
}

// Risk 4: a matching pod on a non-eligible node still anchors its domain.
func TestAffinityAnchorOnIneligibleNodeCounts(t *testing.T) {
	p := appPod("web")
	p.Spec.Affinity = &corev1.Affinity{PodAffinity: &corev1.PodAffinity{
		RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{{
			TopologyKey:   "topology.kubernetes.io/zone",
			LabelSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "cache"}},
		}},
	}}
	full, free := zoneNode("full-a", "a"), zoneNode("free-a", "a")
	placed := []PlacedPod{{Namespace: "default", NodeName: "full-a", Labels: map[string]string{"app": "cache"}}}
	c, _ := podAffinity(p, []*corev1.Node{full, free}, []*corev1.Node{free}, placed)
	if len(c) != 0 {
		t.Fatalf("cache pod in zone a anchors free-a, got: %s", causeText(c))
	}
}

// Risk 5: an empty namespaceSelector means all namespaces.
func TestEmptyNamespaceSelectorMeansAllNamespaces(t *testing.T) {
	p := appPod("web")
	p.Spec.Affinity = &corev1.Affinity{PodAffinity: &corev1.PodAffinity{
		RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{{
			TopologyKey:       "kubernetes.io/hostname",
			LabelSelector:     &metav1.LabelSelector{MatchLabels: map[string]string{"app": "cache"}},
			NamespaceSelector: &metav1.LabelSelector{},
		}},
	}}
	n := readyNode("n1", "8", "16Gi")
	placed := []PlacedPod{{Namespace: "infra", NodeName: "n1", Labels: map[string]string{"app": "cache"}}}
	if c := AnalyzePodAffinity(p, []*corev1.Node{n}, placed); len(c) != 0 {
		t.Fatalf("cache pod in another namespace should satisfy namespaceSelector {}, got: %s", causeText(c))
	}
}

// Risk 6: an existing pod's required anti-affinity rejects the incoming pod.
func TestSymmetricAntiAffinity(t *testing.T) {
	p := appPod("web")
	n := readyNode("n1", "8", "16Gi")
	placed := []PlacedPod{{
		Namespace: "default", Name: "loner", NodeName: "n1", Labels: map[string]string{"app": "db"},
		RequiredAntiAffinity: []corev1.PodAffinityTerm{{
			TopologyKey:   "kubernetes.io/hostname",
			LabelSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "web"}},
		}},
	}}
	r := Analyze(Input{Pod: p, Nodes: []NodeView{{Node: n}}, ClusterPods: placed})
	if !hasCause(r, "Existing pods' anti-affinity") {
		t.Fatalf("want symmetric anti-affinity blocker, got: %s", titles(r))
	}
	if r.Nodes[0].Schedulable || !strings.Contains(r.Nodes[0].Reason, "default/loner") {
		t.Errorf("verdict should name the blocking pod: %+v", r.Nodes[0])
	}
}

// Risk 7: nodes the pod can't use (nodeSelector) are not spread domains.
func TestTopologySpreadHonorsNodeSelector(t *testing.T) {
	p := spreadPod()
	p.Spec.NodeSelector = map[string]string{"pool": "b"}
	nA, nB := zoneNode("n-a", "a"), zoneNode("n-b", "b")
	nB.Labels["pool"] = "b"
	placed := []PlacedPod{{Namespace: "default", NodeName: "n-b", Labels: map[string]string{"app": "web"}}}
	if c := AnalyzeTopologySpread(p, []*corev1.Node{nA, nB}, []*corev1.Node{nB}, placed); len(c) != 0 {
		t.Fatalf("zone a is excluded by nodeAffinityPolicy=Honor, so b is fine; got: %s", causeText(c))
	}
}

// Risk 7: terminating pods are not counted.
func TestTopologySpreadSkipsTerminatingPods(t *testing.T) {
	p := spreadPod()
	nA, nB := zoneNode("n-a", "a"), zoneNode("n-b", "b")
	placed := []PlacedPod{
		{Namespace: "default", NodeName: "n-a", Labels: map[string]string{"app": "web"}},
		{Namespace: "default", NodeName: "n-a", Labels: map[string]string{"app": "web"}, Terminating: true},
		{Namespace: "default", NodeName: "n-b", Labels: map[string]string{"app": "web"}},
	}
	// Only n-a is eligible. Live counts a=1, b=1, so a 2nd pod in a gives skew
	// 1: allowed. Counting the terminating pod (a=2) would wrongly give skew 2.
	if c := AnalyzeTopologySpread(p, []*corev1.Node{nA, nB}, []*corev1.Node{nA}, placed); len(c) != 0 {
		t.Fatalf("terminating pod must not count toward skew, got: %s", causeText(c))
	}
}

// Risk 10: a pod tolerating not-ready / unschedulable can use those nodes.
func TestNotReadyAndCordonTolerations(t *testing.T) {
	p := pod("100m", "64Mi")
	p.Spec.Tolerations = []corev1.Toleration{
		{Key: corev1.TaintNodeNotReady, Operator: corev1.TolerationOpExists},
		{Key: corev1.TaintNodeUnschedulable, Operator: corev1.TolerationOpExists},
	}
	nr := notReadyNode("nr", "4", "8Gi")
	cd := readyNode("cd", "4", "8Gi")
	cd.Spec.Unschedulable = true
	r := Analyze(Input{Pod: p, Nodes: []NodeView{{Node: nr}, {Node: cd}}})
	if r.HasBlocker() {
		t.Fatalf("tolerating pod should fit, got: %s", titles(r))
	}
	for _, v := range r.Nodes {
		if !v.Schedulable {
			t.Errorf("node %s should be schedulable for this pod: %s", v.Name, v.Reason)
		}
	}
}

// Risk 11: verdicts reflect later stages (extended resources here).
func TestVerdictUpdatedAfterExtendedResources(t *testing.T) {
	r := Analyze(Input{Pod: gpuPod(1), Nodes: []NodeView{{Node: readyNode("cpu-only", "8", "32Gi")}}})
	if len(r.Nodes) != 1 || r.Nodes[0].Schedulable {
		t.Fatalf("cpu-only node cannot host a GPU pod: %+v", r.Nodes)
	}
	if !strings.Contains(r.Nodes[0].Reason, "does not advertise") {
		t.Errorf("reason should say the resource is missing: %q", r.Nodes[0].Reason)
	}
}

// Risk 12: WaitForFirstConsumer PVCs are Info, not a Blocker.
func TestWaitForFirstConsumerPVCIsInfo(t *testing.T) {
	r := Analyze(Input{
		Pod:         pod("100m", "64Mi"),
		Nodes:       []NodeView{{Node: readyNode("n1", "4", "8Gi")}},
		WaitingPVCs: []string{"data"},
	})
	if r.HasBlocker() {
		t.Fatalf("WFFC PVC is not a blocker: %s", titles(r))
	}
	if !hasCause(r, "WaitForFirstConsumer") {
		t.Fatalf("want the WFFC info line: %s", titles(r))
	}
}

// Risk 14: unreadable ResourceClaims are "unknown", not "not found".
func TestDRAUnreadableIsNotNotFound(t *testing.T) {
	p := draPod("job-0", "ml", "gpu", "gpu-claim")
	r := Analyze(Input{
		Pod:            p,
		Nodes:          []NodeView{{Node: readyNode("n1", "4", "8Gi")}},
		DRAClaimsError: `resourceclaims.resource.k8s.io is forbidden`,
	})
	if r.HasBlocker() || hasCause(r, "not found") {
		t.Fatalf("unreadable API must not become a 'not found' blocker: %s", titles(r))
	}
	if !hasCause(r, "Couldn't read ResourceClaims") {
		t.Fatalf("want the unknown-status info: %s", titles(r))
	}
}

// Review 2026-11-20 #2: the largest free CPU and the largest free memory sit on
// different nodes. Aggregate is enough, so this is fragmentation, not
// "insufficient". Both nodes 8 CPU / 16Gi; A has 4 CPU / 1Gi free, B has
// 1 CPU / 8Gi free; pod 2 / 4Gi fits either node when empty.
func TestFragmentationAcrossDimensions(t *testing.T) {
	a, b := readyNode("a", "8", "16Gi"), readyNode("b", "8", "16Gi")
	gi := int64(1 << 30)
	r := Analyze(Input{Pod: pod("2", "4Gi"), Nodes: []NodeView{
		{Node: a, Used: Resources{CPUMilli: 4000, MemBytes: 15 * gi}},
		{Node: b, Used: Resources{CPUMilli: 7000, MemBytes: 8 * gi}},
	}})
	if !hasCause(r, "fragmentation") || hasCause(r, "Insufficient free capacity") {
		t.Fatalf("want fragmentation, got: %s", titles(r))
	}
}

// Review #3: sidecars (restartPolicy: Always init containers) are summed with
// the main containers; ordinary init containers still use max.
func TestPodRequests_SidecarsAndOverhead(t *testing.T) {
	always := corev1.ContainerRestartPolicyAlways
	p := pod("1", "1Gi")
	p.Spec.InitContainers = []corev1.Container{
		{Name: "mesh", RestartPolicy: &always, Resources: corev1.ResourceRequirements{Requests: cpuMem("500m", "256Mi")}},
		{Name: "migrate", Resources: corev1.ResourceRequirements{Requests: cpuMem("1", "512Mi")}},
	}
	p.Spec.Overhead = cpuMem("250m", "128Mi")
	got := PodRequests(p)
	// containers 1 + sidecar 0.5 = 1.5; init peak = migrate 1 + sidecar 0.5 = 1.5;
	// max = 1.5, plus overhead 0.25 = 1.75 CPU. Memory: 1Gi+256Mi vs 512+256Mi,
	// max 1280Mi, plus 128Mi = 1408Mi.
	if got.CPUMilli != 1750 {
		t.Errorf("CPU = %dm, want 1750m", got.CPUMilli)
	}
	if got.MemBytes != 1408<<20 {
		t.Errorf("memory = %d, want %d", got.MemBytes, int64(1408<<20))
	}
}

// Review #3: ephemeral-storage requests are checked against allocatable.
func TestEphemeralStorageShortage(t *testing.T) {
	n := readyNode("n1", "8", "16Gi")
	n.Status.Allocatable[corev1.ResourceEphemeralStorage] = resource.MustParse("10Gi")
	p := pod("100m", "64Mi")
	p.Spec.Containers[0].Resources.Requests[corev1.ResourceEphemeralStorage] = resource.MustParse("4Gi")
	r := Analyze(Input{Pod: p, Nodes: []NodeView{{Node: n, Used: Resources{EphemeralBytes: 8 << 30}}}})
	if !r.HasBlocker() || !hasCause(r, "ephemeral-storage") {
		t.Fatalf("want ephemeral-storage blocker, got: %s", titles(r))
	}
	if hasCause(r, "Insufficient free capacity") || hasCause(r, "fragmentation") {
		t.Errorf("CPU/memory fit, must not blame them: %s", titles(r))
	}
}

// Review #3: a node at its max-pods allocatable can't take another pod.
func TestMaxPodsReached(t *testing.T) {
	n := readyNode("n1", "8", "16Gi")
	n.Status.Allocatable[corev1.ResourcePods] = resource.MustParse("110")
	r := Analyze(Input{Pod: pod("100m", "64Mi"), Nodes: []NodeView{{Node: n, Used: Resources{Pods: 110}}}})
	if !hasCause(r, "max-pods") || !r.HasBlocker() {
		t.Fatalf("want max-pods blocker, got: %s", titles(r))
	}
	if !strings.Contains(r.Nodes[0].Reason, "110/110") {
		t.Errorf("verdict should show the count: %q", r.Nodes[0].Reason)
	}
}

// Review #5: a failed read is a visible Warning, not silence.
func TestReadErrorsSurface(t *testing.T) {
	r := Analyze(Input{
		Pod:        pod("100m", "64Mi"),
		Nodes:      []NodeView{{Node: readyNode("n1", "4", "8Gi")}},
		ReadErrors: []string{"PVC data: persistentvolumeclaims \"data\" is forbidden"},
	})
	if !hasCause(r, "Couldn't read some objects") || r.HasBlocker() {
		t.Fatalf("want a read-error warning only, got: %s", titles(r))
	}
}

// Live-judge cluster shape (report 2026-11-20): 3 control-plane nodes tainted
// node-role.kubernetes.io/etcd:NoExecute, 3 untainted workers.
func judgeCluster() []NodeView {
	var out []NodeView
	for _, n := range []string{"cp-1", "cp-2", "cp-3"} {
		node := readyNode(n, "16", "32Gi")
		node.Spec.Taints = []corev1.Taint{{Key: "node-role.kubernetes.io/etcd", Value: "true", Effect: corev1.TaintEffectNoExecute}}
		out = append(out, NodeView{Node: node})
	}
	for _, n := range []string{"w-1", "w-2", "w-3"} {
		out = append(out, NodeView{Node: readyNode(n, "8", "16Gi")})
	}
	return out
}

// Report scenario 4: pinned to a tainted control-plane node. The answer is that
// node's taint, named; the selector is context.
func TestPinnedToTaintedNodeNamesTheNode(t *testing.T) {
	p := pod("1m", "8Mi")
	p.Spec.NodeSelector = map[string]string{"kubernetes.io/hostname": "cp-1"}
	r := Analyze(Input{Pod: p, Nodes: judgeCluster()})
	if len(r.Causes) == 0 || r.Causes[0].Severity != Blocker || !strings.Contains(r.Causes[0].Detail, "cp-1 (node-role.kubernetes.io/etcd=true:NoExecute)") {
		t.Fatalf("top cause should name cp-1 and its taint, got: %s / %+v", titles(r), r.Causes)
	}
	for _, c := range r.Causes {
		if strings.Contains(c.Title, "nodeSelector") && c.Severity == Blocker {
			t.Errorf("selector matched a node; it must not be a Blocker: %s", titles(r))
		}
	}
}

// Report scenario 20: pinned to a cordoned worker. Cordon is the blocker.
func TestPinnedToCordonedNodeIsBlocker(t *testing.T) {
	nodes := judgeCluster()
	nodes[3].Node.Spec.Unschedulable = true // w-1
	nodes[3].Node.Spec.Taints = []corev1.Taint{{Key: corev1.TaintNodeUnschedulable, Effect: corev1.TaintEffectNoSchedule}}
	p := pod("1m", "8Mi")
	p.Spec.NodeSelector = map[string]string{"kubernetes.io/hostname": "w-1"}
	r := Analyze(Input{Pod: p, Nodes: nodes})
	if len(r.HasBlockerCauses()) != 1 || !strings.Contains(r.Causes[0].Title, "cordoned") {
		t.Fatalf("cordon should be the single blocker, got: %s", titles(r))
	}
}

// Report scenario 11: hostname spread where the only empty domains are the
// tainted control-plane nodes. Fix must be nodeTaintsPolicy: Honor, not
// "add a node".
func TestSpreadBlockedByTaintedEmptyDomains(t *testing.T) {
	p := appPod("spread")
	p.Spec.TopologySpreadConstraints = []corev1.TopologySpreadConstraint{{
		MaxSkew: 1, TopologyKey: "kubernetes.io/hostname", WhenUnsatisfiable: corev1.DoNotSchedule,
		LabelSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "spread"}},
	}}
	var placed []PlacedPod
	for _, w := range []string{"w-1", "w-2", "w-3"} {
		placed = append(placed, PlacedPod{Namespace: "default", NodeName: w, Labels: map[string]string{"app": "spread"}})
	}
	r := Analyze(Input{Pod: p, Nodes: judgeCluster(), ClusterPods: placed})
	var spread *Cause
	for i := range r.Causes {
		if strings.Contains(r.Causes[i].Title, "Topology spread") {
			spread = &r.Causes[i]
		}
	}
	if spread == nil || !strings.Contains(spread.Fix, "nodeTaintsPolicy: Honor") {
		t.Fatalf("want spread blocker recommending nodeTaintsPolicy: Honor, got: %+v", r.Causes)
	}
}

// Report 2.2: a warning (tainted control plane) must not hide the verdict line.
func TestWarningsDoNotSuppressVerdictLine(t *testing.T) {
	r := Analyze(Input{Pod: pod("1m", "8Mi"), Nodes: judgeCluster()})
	if !hasCause(r, "should fit") {
		t.Fatalf("want an explicit 'should fit' line next to the taint warning, got: %s", titles(r))
	}
}

// HasBlockerCauses returns just the blockers (test helper).
func (r Result) HasBlockerCauses() []Cause {
	var out []Cause
	for _, c := range r.Causes {
		if c.Severity == Blocker {
			out = append(out, c)
		}
	}
	return out
}

// Retest #2: replicas sharing one anti-affinity rule get ONE blocker, not the
// own-term blocker plus a "symmetric" duplicate.
func TestSharedAntiAffinityReportedOnce(t *testing.T) {
	term := corev1.PodAffinityTerm{
		TopologyKey:   "kubernetes.io/hostname",
		LabelSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "oph"}},
	}
	p := appPod("oph")
	p.Spec.Affinity = &corev1.Affinity{PodAntiAffinity: &corev1.PodAntiAffinity{
		RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{term},
	}}
	var nodes []NodeView
	var placed []PlacedPod
	for _, n := range []string{"w-1", "w-2"} {
		nodes = append(nodes, NodeView{Node: readyNode(n, "8", "16Gi")})
		placed = append(placed, PlacedPod{Namespace: "default", Name: "oph-" + n, NodeName: n,
			Labels: map[string]string{"app": "oph"}, RequiredAntiAffinity: []corev1.PodAffinityTerm{term}})
	}
	r := Analyze(Input{Pod: p, Nodes: nodes, ClusterPods: placed})
	if n := len(r.HasBlockerCauses()); n != 1 {
		t.Fatalf("want exactly one anti-affinity blocker, got %d: %s", n, titles(r))
	}
}

// Retest open item: never tell users to untaint control-plane/etcd nodes.
func TestControlPlaneTaintAdviceKeepsTaint(t *testing.T) {
	p := pod("1m", "8Mi")
	p.Spec.NodeSelector = map[string]string{"kubernetes.io/hostname": "cp-1"}
	r := Analyze(Input{Pod: p, Nodes: judgeCluster()})
	fix := r.Causes[0].Fix
	if strings.Contains(fix, "kubectl taint nodes") || !strings.Contains(fix, "keep them") {
		t.Fatalf("control-plane taint advice should keep the taint, got: %q", fix)
	}
}

// Retest open item: missing DeviceClass is named in the title.
func TestDRAMissingClassTitle(t *testing.T) {
	p := draPod("job-0", "ml", "gpu", "gpu-claim")
	claims := []resourcev1.ResourceClaim{draClaim("ml", "gpu-claim", "fake.example.com", false)}
	c := AnalyzeDRA(p, claims, []resourcev1.ResourceSlice{}, []resourcev1.DeviceClass{}, nil)
	if len(c) != 1 || !strings.Contains(c[0].Title, "fake.example.com, which doesn't exist") {
		t.Fatalf("title should name the missing class: %+v", c)
	}
}

// Retest #1: an unreadable StorageClass is a Warning, never a Blocker.
func TestUnknownPVCBindingIsWarning(t *testing.T) {
	r := Analyze(Input{
		Pod:         pod("100m", "64Mi"),
		Nodes:       []NodeView{{Node: readyNode("n1", "4", "8Gi")}},
		UnknownPVCs: []string{`data (couldn't read StorageClass "local-path": forbidden)`},
	})
	if r.HasBlocker() || !hasCause(r, "couldn't read the StorageClass") {
		t.Fatalf("want a warning, not a blocker: %s", titles(r))
	}
}

// Review 3 B1: per-dimension maxima from different nodes must not make a pod
// look like it fits "on an empty node". 16 CPU / 8Gi + 4 CPU / 64Gi, pod
// 12 CPU / 32Gi: no node can ever hold it, so no "defragment" advice.
func TestTooBigMixedShapes(t *testing.T) {
	r := Analyze(Input{Pod: pod("12", "32Gi"), Nodes: []NodeView{
		{Node: readyNode("cpu-heavy", "16", "8Gi")}, {Node: readyNode("mem-heavy", "4", "64Gi")},
	}})
	if !hasCause(r, "larger than any single node") || hasCause(r, "fragmentation") {
		t.Fatalf("want too-big, got: %s", titles(r))
	}
}

// Review 3 B2: a stale nomination must not downgrade a selector blocker.
func TestStaleNominationKeepsBlocker(t *testing.T) {
	p := pod("100m", "64Mi")
	p.Spec.NodeSelector = map[string]string{"disk": "nvme"}
	p.Status.NominatedNodeName = "n1"
	r := Analyze(Input{Pod: p, Nodes: []NodeView{{Node: readyNode("n1", "4", "8Gi")}}})
	if !r.HasBlocker() {
		t.Fatalf("stale nomination hid the selector blocker: %s", titles(r))
	}
	// A viable nomination (capacity only, preemption in flight) still downgrades.
	q := pod("6", "64Mi")
	q.Status.NominatedNodeName = "n1"
	r = Analyze(Input{Pod: q, Nodes: []NodeView{{Node: readyNode("n1", "8", "8Gi"), Used: Resources{CPUMilli: 4000}}}})
	if r.HasBlocker() {
		t.Fatalf("viable nomination should downgrade: %s", titles(r))
	}
}

// Review 3 B3: bound to a node that doesn't exist.
func TestBoundToMissingNode(t *testing.T) {
	p := pod("100m", "64Mi")
	p.Spec.NodeName = "ghost"
	r := Analyze(Input{Pod: p, Nodes: []NodeView{{Node: readyNode("n1", "4", "8Gi")}}})
	if !r.HasBlocker() || !hasCause(r, "doesn't exist") {
		t.Fatalf("want missing-node blocker: %s", titles(r))
	}
}

// Review 3 B6: custom scheduler that never acted; and the default-scheduler
// silence warning.
func TestSchedulerSilence(t *testing.T) {
	now := time.Now()
	p := pod("100m", "64Mi")
	p.CreationTimestamp = metav1.NewTime(now.Add(-5 * time.Minute))
	p.Spec.SchedulerName = "ghost-scheduler"
	nodes := []NodeView{{Node: readyNode("n1", "4", "8Gi")}}
	r := Analyze(Input{Pod: p, Nodes: nodes, Now: now})
	if !r.HasBlocker() || !hasCause(r, `"ghost-scheduler" has never tried`) {
		t.Fatalf("want custom scheduler blocker: %s", titles(r))
	}
	p.Spec.SchedulerName = ""
	r = Analyze(Input{Pod: p, Nodes: nodes, Now: now})
	if !hasCause(r, "No scheduling events yet") {
		t.Fatalf("want silence warning: %s", titles(r))
	}
	r = Analyze(Input{Pod: p, Nodes: nodes, Now: now, SchedulerEventSeen: true})
	if hasCause(r, "No scheduling events") || hasCause(r, "never tried") {
		t.Fatalf("event seen, no silence cause expected: %s", titles(r))
	}
}

// Review 3 G1 + B11: pod-level resources and in-place resize allocations.
func TestPodRequests_PodLevelAndResize(t *testing.T) {
	p := pod("0", "0")
	p.Spec.Resources = &corev1.ResourceRequirements{Requests: corev1.ResourceList{
		corev1.ResourceCPU: resource.MustParse("7600m"), corev1.ResourceMemory: resource.MustParse("64Mi"),
	}}
	if r := PodRequests(p); r.CPUMilli != 7600 || r.MemBytes != 64<<20 {
		t.Fatalf("pod-level resources ignored: %+v", r)
	}
	q := pod("1", "1Gi")
	q.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: q.Spec.Containers[0].Name,
		AllocatedResources: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("3")}}}
	if r := PodRequests(q); r.CPUMilli != 3000 || r.MemBytes != 1<<30 {
		t.Fatalf("resize allocation not maxed in: %+v", r)
	}
}
