package diagnose

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func zoneNode(name, zone string) *corev1.Node {
	n := readyNode(name, "8", "16Gi")
	n.Labels["topology.kubernetes.io/zone"] = zone
	return n
}

func appPod(app string) *corev1.Pod {
	p := pod("100m", "64Mi")
	p.Labels = map[string]string{"app": app}
	return p
}

func spreadPod() *corev1.Pod {
	p := appPod("web")
	p.Spec.TopologySpreadConstraints = []corev1.TopologySpreadConstraint{{
		MaxSkew:           1,
		TopologyKey:       "topology.kubernetes.io/zone",
		WhenUnsatisfiable: corev1.DoNotSchedule,
		LabelSelector:     &metav1.LabelSelector{MatchLabels: map[string]string{"app": "web"}},
	}}
	return p
}

func TestTopologySpreadBlocks(t *testing.T) {
	// 3 zones, maxSkew=1. zone-a=1, zone-b=1, zone-c=0 — the pod belongs in c to
	// balance, but zone-c's node isn't eligible (full/tainted), so a/b would go
	// to 2 while c stays 0: skew 2 > 1. Blocked.
	p := spreadPod()
	nA, nB, nC := zoneNode("n-a", "a"), zoneNode("n-b", "b"), zoneNode("n-c", "c")
	allNodes := []*corev1.Node{nA, nB, nC}
	eligible := []*corev1.Node{nA, nB} // n-c not schedulable
	placed := []PlacedPod{
		{Namespace: "default", NodeName: "n-a", Labels: map[string]string{"app": "web"}},
		{Namespace: "default", NodeName: "n-b", Labels: map[string]string{"app": "web"}},
	}
	if c := AnalyzeTopologySpread(p, allNodes, eligible, placed); len(c) == 0 {
		t.Fatal("expected a topology spread blocker")
	}
}

func TestTopologySpreadSatisfiable(t *testing.T) {
	// Two zones, one matching pod in a, zone-b empty and eligible — placing in b
	// keeps skew at 1. OK.
	p := spreadPod()
	nA, nB := zoneNode("n-a", "a"), zoneNode("n-b", "b")
	allNodes := []*corev1.Node{nA, nB}
	placed := []PlacedPod{
		{Namespace: "default", NodeName: "n-a", Labels: map[string]string{"app": "web"}},
	}
	if c := AnalyzeTopologySpread(p, allNodes, allNodes, placed); len(c) != 0 {
		t.Fatalf("expected satisfiable, got: %+v", c)
	}
}

func TestAntiAffinityRunsOutOfHosts(t *testing.T) {
	// "one web per host" with both hosts already running a web pod.
	p := appPod("web")
	p.Spec.Affinity = &corev1.Affinity{PodAntiAffinity: &corev1.PodAntiAffinity{
		RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{{
			TopologyKey:   "kubernetes.io/hostname",
			LabelSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "web"}},
		}},
	}}
	eligible := []*corev1.Node{readyNode("n1", "8", "16Gi"), readyNode("n2", "8", "16Gi")}
	placed := []PlacedPod{
		{Namespace: "default", NodeName: "n1", Labels: map[string]string{"app": "web"}},
		{Namespace: "default", NodeName: "n2", Labels: map[string]string{"app": "web"}},
	}
	causes := AnalyzePodAffinity(p, eligible, placed)
	if len(causes) == 0 {
		t.Fatal("expected anti-affinity blocker")
	}
}

func TestPodAffinityNoMatchingPod(t *testing.T) {
	// Pod must co-locate with app=cache, but no cache pod exists anywhere.
	p := appPod("web")
	p.Spec.Affinity = &corev1.Affinity{PodAffinity: &corev1.PodAffinity{
		RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{{
			TopologyKey:   "kubernetes.io/hostname",
			LabelSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "cache"}},
		}},
	}}
	n := readyNode("n1", "8", "16Gi")
	causes := AnalyzePodAffinity(p, []*corev1.Node{n}, nil)
	if len(causes) == 0 {
		t.Fatal("expected pod affinity blocker")
	}
}

func TestLabelSelectorExpressions(t *testing.T) {
	sel := &metav1.LabelSelector{
		MatchExpressions: []metav1.LabelSelectorRequirement{
			{Key: "tier", Operator: metav1.LabelSelectorOpIn, Values: []string{"frontend", "web"}},
			{Key: "deprecated", Operator: metav1.LabelSelectorOpDoesNotExist},
		},
	}
	if !LabelSelectorMatches(sel, map[string]string{"tier": "web"}) {
		t.Error("should match tier=web with no deprecated label")
	}
	if LabelSelectorMatches(sel, map[string]string{"tier": "web", "deprecated": "true"}) {
		t.Error("should not match when deprecated label present")
	}
	if LabelSelectorMatches(sel, map[string]string{"tier": "backend"}) {
		t.Error("should not match tier=backend")
	}
}
