package diagnose

import (
	"strconv"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

// Real FailedScheduling messages, by the release whose wording they use.
func TestParseSchedulerMessage_Versions(t *testing.T) {
	cases := []struct {
		name, msg string
		total     int
		want      []string // "count category"
	}{
		{"1.26-1.33 taint wording",
			"0/6 nodes are available: 1 node(s) didn't match Pod's node affinity/selector, 3 node(s) had untolerated taint {node-role.kubernetes.io/etcd: true}, 2 Insufficient cpu. preemption: 0/6 nodes are available: 3 Preemption is not helpful for scheduling, 3 No preemption victims found for incoming pod.",
			6, []string{"1 placement", "3 taint", "2 cpu"}},
		{"1.35 taint(s) wording, comma inside braces",
			"0/5 nodes are available: 2 node(s) had untolerated taint(s) {dedicated: gpu, team: ml}, 3 Insufficient memory.",
			5, []string{"2 taint", "3 memory"}},
		{"not-ready taint",
			"0/3 nodes are available: 1 node(s) had untolerated taint {node.kubernetes.io/not-ready: }, 2 node(s) were unschedulable.",
			3, []string{"1 not-ready", "2 cordoned"}},
		{"hostPort (live review 3 R2)",
			"0/6 nodes are available: 3 node(s) didn't have free ports for the requested pod ports, 3 node(s) had untolerated taint {node-role.kubernetes.io/etcd: true}. preemption: 0/6 nodes are available: 6 Preemption is not helpful for scheduling.",
			6, []string{"3 host-port", "3 taint"}},
		{"PV node affinity (live review 3 R3)",
			"0/6 nodes are available: 3 node(s) had untolerated taint {node-role.kubernetes.io/etcd: true}, 3 node(s) had volume node affinity conflict.",
			6, []string{"3 taint", "3 volume-topology"}},
		{"PreFilter, no counts",
			"0/3 nodes are available: pod has unbound immediate PersistentVolumeClaims. preemption: 0/3 nodes are available: 3 Preemption is not helpful for scheduling.",
			3, []string{"0 pvc"}},
		{"spread, affinity, extended, max pods, attach limits",
			"0/7 nodes are available: 1 Insufficient nvidia.com/gpu, 1 Too many pods, 1 node(s) didn't match pod topology spread constraints, 1 node(s) didn't match pod anti-affinity rules, 1 node(s) didn't satisfy existing pods anti-affinity rules, 1 node(s) exceed max volume count, 1 node(s) had untolerated device taint.",
			7, []string{"1 extended-resource", "1 max-pods", "1 topology-spread", "1 pod-affinity", "1 pod-affinity", "1 volume-attach-limit", "1 dra"}},
		{"DRA (1.34+)",
			"0/3 nodes are available: 3 cannot allocate all claims.",
			3, []string{"3 dra"}},
		{"unknown future reason",
			"0/2 nodes are available: 2 node(s) failed the frobnicator check.",
			2, []string{"2 other"}},
	}
	for _, c := range cases {
		p, ok := ParseSchedulerMessage(c.msg)
		if !ok || p.Total != c.total {
			t.Errorf("%s: ok=%v total=%d", c.name, ok, p.Total)
			continue
		}
		var got []string
		for _, r := range p.Reasons {
			got = append(got, strings.Join([]string{itoa(r.Count), r.Category}, " "))
		}
		if strings.Join(got, ",") != strings.Join(c.want, ",") {
			t.Errorf("%s:\n got  %v\n want %v", c.name, got, c.want)
		}
	}
	for _, bad := range []string{"", "skip schedule deleting pod: default/x", "binding rejected: running Bind plugin", "pod is gated"} {
		if _, ok := ParseSchedulerMessage(bad); ok {
			t.Errorf("%q should not parse", bad)
		}
	}
}

func itoa(n int) string { return strconv.Itoa(n) }

// Live review 3 R2: hostPort held on every worker. Before the cross-check the
// tool said "3 nodes should fit", exit 0; now it's a Blocker naming hostPorts.
func TestCrossCheck_UnmodelledBecomesBlocker(t *testing.T) {
	r := Analyze(Input{
		Pod:            pod("100m", "64Mi"),
		Nodes:          judgeCluster(),
		SchedulerEvent: "0/6 nodes are available: 3 node(s) didn't have free ports for the requested pod ports, 3 node(s) had untolerated taint {node-role.kubernetes.io/etcd: true}.",
	})
	if !r.HasBlocker() || !hasCause(r, "free ports") || hasCause(r, "should fit") {
		t.Fatalf("want a hostPort blocker, no 'should fit': %s", titles(r))
	}
	if len(r.SchedulerReasons) != 2 {
		t.Fatalf("reasons not exposed: %+v", r.SchedulerReasons)
	}
}

// A modelled filter where the model disagrees: labelled as a disagreement.
func TestCrossCheck_ModelledDisagreement(t *testing.T) {
	r := Analyze(Input{
		Pod:            pod("100m", "64Mi"),
		Nodes:          []NodeView{{Node: readyNode("n1", "4", "8Gi")}},
		SchedulerEvent: "0/1 nodes are available: 1 Insufficient cpu.",
	})
	if !r.HasBlocker() || !hasCause(r, "model disagrees") {
		t.Fatalf("want a disagreement blocker: %s", titles(r))
	}
}

// When this tool already has a blocker, unmodelled scheduler reasons add one
// warning; modelled ones add nothing (order differences aren't flagged).
func TestCrossCheck_ExtraReasonWithBlocker(t *testing.T) {
	r := Analyze(Input{
		Pod:            pod("64", "64Mi"),
		Nodes:          []NodeView{{Node: readyNode("n1", "4", "8Gi")}, {Node: readyNode("n2", "4", "8Gi")}},
		SchedulerEvent: "0/2 nodes are available: 1 Insufficient cpu, 1 node(s) had volume node affinity conflict.",
	})
	if !hasCause(r, "larger than any single node") || !hasCause(r, "also reports a reason") || hasCause(r, "model disagrees") {
		t.Fatalf("got: %s", titles(r))
	}
	r = Analyze(Input{
		Pod:            pod("64", "64Mi"),
		Nodes:          []NodeView{{Node: readyNode("n1", "4", "8Gi")}},
		SchedulerEvent: "0/1 nodes are available: 1 node(s) didn't match Pod's node affinity/selector.",
	})
	if hasCause(r, "also reports") || hasCause(r, "Scheduler:") {
		t.Fatalf("modelled reasons must not add causes when a blocker exists: %s", titles(r))
	}
}

// Gated / bound pods never reach the cross-check (triage returns first).
func TestCrossCheck_SkippedForTriagedPods(t *testing.T) {
	p := pod("100m", "64Mi")
	p.Spec.SchedulingGates = []corev1.PodSchedulingGate{{Name: "x"}}
	r := Analyze(Input{Pod: p, Nodes: judgeCluster(), SchedulerEvent: "0/6 nodes are available: 6 Insufficient cpu."})
	if r.HasBlocker() {
		t.Fatalf("gated pod got a blocker: %s", titles(r))
	}
}
