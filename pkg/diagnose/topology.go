package diagnose

import (
	"fmt"
	"sort"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// PlacedPod is a pod already running on a node — the cross-pod context needed to
// reason about topology spread and inter-pod (anti-)affinity.
type PlacedPod struct {
	Namespace string
	NodeName  string
	Labels    map[string]string
}

// LabelSelectorMatches reports whether labels satisfy a metav1.LabelSelector.
// A nil selector matches nothing; an empty selector matches everything (the
// semantics Kubernetes uses for pod affinity terms).
func LabelSelectorMatches(sel *metav1.LabelSelector, labels map[string]string) bool {
	if sel == nil {
		return false
	}
	for k, v := range sel.MatchLabels {
		if labels[k] != v {
			return false
		}
	}
	for _, e := range sel.MatchExpressions {
		val, ok := labels[e.Key]
		switch e.Operator {
		case metav1.LabelSelectorOpIn:
			if !ok || !contains(e.Values, val) {
				return false
			}
		case metav1.LabelSelectorOpNotIn:
			if ok && contains(e.Values, val) {
				return false
			}
		case metav1.LabelSelectorOpExists:
			if !ok {
				return false
			}
		case metav1.LabelSelectorOpDoesNotExist:
			if ok {
				return false
			}
		}
	}
	return true
}

// nodeDomain returns the value of a node's topology label (the "domain" the node
// belongs to for a given topologyKey), and whether the node carries that label.
func nodeDomain(node *corev1.Node, topologyKey string) (string, bool) {
	v, ok := node.Labels[topologyKey]
	return v, ok
}

// AnalyzeTopologySpread checks required (DoNotSchedule) pod topology spread
// constraints. The global skew is computed over ALL domains (so an under-filled
// domain with no schedulable node still counts toward the minimum), but the pod
// may only land in an ELIGIBLE domain — one with a node that passed every other
// filter. If no eligible domain keeps the skew within maxSkew, it's the blocker.
//
// Best-effort: implements the common maxSkew/topologyKey/labelSelector case;
// minDomains, nodeAffinityPolicy, and matchLabelKeys are not modeled.
func AnalyzeTopologySpread(pod *corev1.Pod, allNodes, eligible []*corev1.Node, placed []PlacedPod) []Cause {
	eligibleNames := map[string]bool{}
	for _, n := range eligible {
		eligibleNames[n.Name] = true
	}

	var causes []Cause
	for _, c := range pod.Spec.TopologySpreadConstraints {
		if c.WhenUnsatisfiable != corev1.DoNotSchedule {
			continue // ScheduleAnyway is soft, never blocks
		}
		// Every domain present in the cluster (by the topology key), plus which
		// domains contain at least one eligible node.
		nodeByName := map[string]*corev1.Node{}
		domains := map[string]int{}
		eligibleDomains := map[string]bool{}
		for _, n := range allNodes {
			d, ok := nodeDomain(n, c.TopologyKey)
			if !ok {
				continue
			}
			nodeByName[n.Name] = n
			if _, seen := domains[d]; !seen {
				domains[d] = 0
			}
			if eligibleNames[n.Name] {
				eligibleDomains[d] = true
			}
		}
		if len(domains) == 0 {
			causes = append(causes, Cause{
				Severity: Blocker,
				Title:    fmt.Sprintf("No nodes carry the topology key %q", c.TopologyKey),
				Detail:   "A required topologySpreadConstraint references a topology key that no node is labeled with, so the pod can never satisfy it.",
				Fix:      fmt.Sprintf("Label nodes with %s=<domain>, or correct the constraint's topologyKey.", c.TopologyKey),
			})
			continue
		}
		for _, p := range placed {
			n, ok := nodeByName[p.NodeName]
			if !ok || p.Namespace != pod.Namespace {
				continue
			}
			if LabelSelectorMatches(c.LabelSelector, p.Labels) {
				d, _ := nodeDomain(n, c.TopologyKey)
				domains[d]++
			}
		}
		globalMin := int(^uint(0) >> 1)
		for _, n := range domains {
			if n < globalMin {
				globalMin = n
			}
		}
		// Feasible only if some ELIGIBLE domain keeps skew <= maxSkew once we add the pod.
		feasible := false
		for d := range eligibleDomains {
			if int32(domains[d]+1-globalMin) <= c.MaxSkew {
				feasible = true
				break
			}
		}
		if !feasible {
			causes = append(causes, Cause{
				Severity: Blocker,
				Title:    fmt.Sprintf("Topology spread constraint can't be satisfied (key %q, maxSkew %d)", c.TopologyKey, c.MaxSkew),
				Detail: fmt.Sprintf(
					"Matching pods are distributed across the %q domains as %s. The least-filled domain has %d, but no domain with a schedulable node can take this pod without pushing the skew above maxSkew=%d.",
					c.TopologyKey, formatDomains(domains), globalMin, c.MaxSkew),
				Fix: "Add a node in the least-populated domain (the under-filled zone/host the spread wants to balance into), raise maxSkew, or relax whenUnsatisfiable to ScheduleAnyway.",
			})
		}
	}
	return causes
}

// AnalyzePodAffinity checks required inter-pod affinity and anti-affinity:
// affinity needs a domain that already holds a matching pod; anti-affinity needs
// a domain that holds none.
func AnalyzePodAffinity(pod *corev1.Pod, eligible []*corev1.Node, placed []PlacedPod) []Cause {
	if pod.Spec.Affinity == nil {
		return nil
	}
	var causes []Cause

	domainsWithMatch := func(sel *metav1.LabelSelector, topologyKey string, namespaces []string) (with, total int) {
		ns := namespaces
		if len(ns) == 0 {
			ns = []string{pod.Namespace}
		}
		nsSet := map[string]bool{}
		for _, n := range ns {
			nsSet[n] = true
		}
		nodeByName := map[string]*corev1.Node{}
		allDomains := map[string]bool{}
		for _, n := range eligible {
			if d, ok := nodeDomain(n, topologyKey); ok {
				nodeByName[n.Name] = n
				allDomains[d] = true
			}
		}
		matched := map[string]bool{}
		for _, p := range placed {
			n, ok := nodeByName[p.NodeName]
			if !ok || !nsSet[p.Namespace] {
				continue
			}
			if LabelSelectorMatches(sel, p.Labels) {
				d, _ := nodeDomain(n, topologyKey)
				matched[d] = true
			}
		}
		return len(matched), len(allDomains)
	}

	if aff := pod.Spec.Affinity.PodAffinity; aff != nil {
		for _, t := range aff.RequiredDuringSchedulingIgnoredDuringExecution {
			with, total := domainsWithMatch(t.LabelSelector, t.TopologyKey, t.Namespaces)
			if total == 0 || with == 0 {
				causes = append(causes, Cause{
					Severity: Blocker,
					Title:    fmt.Sprintf("Required pod affinity unsatisfied (topology %q)", t.TopologyKey),
					Detail:   "The pod must be co-located (same topology domain) with pods matching its affinity selector, but no eligible domain currently contains such a pod.",
					Fix:      "Schedule a matching pod first, relax the affinity to preferred, or ensure nodes carry the topology key.",
				})
			}
		}
	}

	if anti := pod.Spec.Affinity.PodAntiAffinity; anti != nil {
		for _, t := range anti.RequiredDuringSchedulingIgnoredDuringExecution {
			with, total := domainsWithMatch(t.LabelSelector, t.TopologyKey, t.Namespaces)
			if total > 0 && with >= total {
				causes = append(causes, Cause{
					Severity: Blocker,
					Title:    fmt.Sprintf("Required pod anti-affinity unsatisfied (topology %q)", t.TopologyKey),
					Detail:   fmt.Sprintf("The pod must avoid domains that already hold a matching pod, but all %d eligible %q domain(s) already contain one. This is the classic 'one replica per host' that runs out of hosts.", total, t.TopologyKey),
					Fix:      "Add a node in a fresh topology domain, or relax the anti-affinity to preferredDuringScheduling.",
				})
			}
		}
	}
	return causes
}

func formatDomains(domains map[string]int) string {
	keys := make([]string, 0, len(domains))
	for k := range domains {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := ""
	for i, k := range keys {
		if i > 0 {
			out += ", "
		}
		out += fmt.Sprintf("%s=%d", k, domains[k])
	}
	return out
}
