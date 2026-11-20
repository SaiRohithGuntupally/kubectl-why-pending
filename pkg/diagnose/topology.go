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
	Name      string
	NodeName  string
	Labels    map[string]string
	// Terminating pods (deletionTimestamp set) are skipped by the scheduler's
	// topology spread counting.
	Terminating bool
	// RequiredAntiAffinity is this pod's own required anti-affinity, which the
	// scheduler enforces against incoming pods too (symmetry).
	RequiredAntiAffinity []corev1.PodAffinityTerm
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

// AnalyzeTopologySpread explains a required (DoNotSchedule) topology spread
// constraint that no eligible node can satisfy.
func AnalyzeTopologySpread(pod *corev1.Pod, allNodes, eligible []*corev1.Node, placed []PlacedPod) []Cause {
	c, _ := topologySpread(pod, allNodes, eligible, placed)
	return c
}

// topologySpread mirrors the PodTopologySpread filter for hard constraints and
// also returns, per eligible node, why that node is rejected (for NodeVerdicts).
//
// Domain census follows the scheduler defaults: nodeAffinityPolicy=Honor (nodes
// failing the pod's nodeSelector / required nodeAffinity are not domains),
// nodeTaintsPolicy=Ignore (tainted nodes still count), and terminating pods are
// not counted. minDomains and matchLabelKeys are not modelled yet.
func topologySpread(pod *corev1.Pod, allNodes, eligible []*corev1.Node, placed []PlacedPod) ([]Cause, map[string]string) {
	reasons := map[string]string{}
	var causes []Cause
	for _, c := range pod.Spec.TopologySpreadConstraints {
		if c.WhenUnsatisfiable != corev1.DoNotSchedule {
			continue // ScheduleAnyway is soft, never blocks
		}
		honorAffinity := c.NodeAffinityPolicy == nil || *c.NodeAffinityPolicy == corev1.NodeInclusionPolicyHonor
		honorTaints := c.NodeTaintsPolicy != nil && *c.NodeTaintsPolicy == corev1.NodeInclusionPolicyHonor

		nodeByName := map[string]*corev1.Node{}
		domains := map[string]int{}
		for _, n := range allNodes {
			d, ok := nodeDomain(n, c.TopologyKey)
			if !ok {
				continue
			}
			if honorAffinity && (!MatchesNodeSelector(pod, n) || !MatchesNodeAffinity(pod, n)) {
				continue
			}
			if honorTaints && len(UntoleratedTaints(pod, n)) > 0 {
				continue
			}
			nodeByName[n.Name] = n
			if _, seen := domains[d]; !seen {
				domains[d] = 0
			}
		}
		if len(domains) == 0 {
			causes = append(causes, Cause{
				Severity: Blocker,
				Title:    fmt.Sprintf("No nodes carry the topology key %q", c.TopologyKey),
				Detail:   "A required topologySpreadConstraint references a topology key that no candidate node is labeled with, so the pod can never satisfy it.",
				Fix:      fmt.Sprintf("Label nodes with %s=<domain>, or correct the constraint's topologyKey.", c.TopologyKey),
			})
			for _, n := range eligible {
				setReason(reasons, n.Name, fmt.Sprintf("no %s label", c.TopologyKey))
			}
			continue
		}
		for _, p := range placed {
			if p.Terminating || p.Namespace != pod.Namespace {
				continue
			}
			n, ok := nodeByName[p.NodeName]
			if !ok {
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
		feasible := false
		for _, n := range eligible {
			d, ok := nodeDomain(n, c.TopologyKey)
			if !ok {
				setReason(reasons, n.Name, fmt.Sprintf("no %s label", c.TopologyKey))
				continue
			}
			if int32(domains[d]+1-globalMin) <= c.MaxSkew {
				feasible = true
				continue
			}
			setReason(reasons, n.Name, fmt.Sprintf("topology spread: %s=%s would exceed maxSkew %d", c.TopologyKey, d, c.MaxSkew))
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
	return causes, reasons
}

// AnalyzePodAffinity explains required inter-pod affinity / anti-affinity that
// no eligible node satisfies. Kept for callers that only know eligible nodes;
// Analyze uses podAffinity with the full node list.
func AnalyzePodAffinity(pod *corev1.Pod, eligible []*corev1.Node, placed []PlacedPod) []Cause {
	c, _ := podAffinity(pod, eligible, eligible, placed)
	return c
}

// termNamespaces resolves which namespaces a term's selector applies to:
// explicit namespaces, all namespaces for an empty namespaceSelector, else the
// pod's own. ok=false for a non-empty namespaceSelector, which needs namespace
// labels this tool doesn't fetch; callers skip such terms rather than guess.
func termNamespaces(pod *corev1.Pod, t corev1.PodAffinityTerm) (set map[string]bool, all, ok bool) {
	if t.NamespaceSelector != nil {
		if len(t.NamespaceSelector.MatchLabels) == 0 && len(t.NamespaceSelector.MatchExpressions) == 0 {
			return nil, true, true
		}
		if len(t.Namespaces) == 0 {
			return nil, false, false
		}
	}
	set = map[string]bool{}
	for _, n := range t.Namespaces {
		set[n] = true
	}
	if len(set) == 0 {
		set[pod.Namespace] = true
	}
	return set, false, true
}

func termMatches(pod *corev1.Pod, t corev1.PodAffinityTerm, ns string, labels map[string]string) bool {
	set, all, _ := termNamespaces(pod, t)
	if !all && !set[ns] {
		return false
	}
	return LabelSelectorMatches(t.LabelSelector, labels)
}

// podAffinity mirrors the InterPodAffinity filter for required terms:
//   - affinity: some eligible node's domain must already hold a matching pod
//     (matching pods on ANY node count, not just eligible ones). Exception: if
//     no pod in the cluster matches any term and the pod matches all its own
//     terms, it's allowed (first replica of "co-locate with my own app").
//   - anti-affinity: a node is rejected if its domain holds a matching pod.
//   - symmetric anti-affinity: a node is rejected if a pod already in its
//     domain has required anti-affinity that matches the incoming pod.
func podAffinity(pod *corev1.Pod, allNodes, eligible []*corev1.Node, placed []PlacedPod) ([]Cause, map[string]string) {
	reasons := map[string]string{}
	var causes []Cause
	nodeByName := map[string]*corev1.Node{}
	for _, n := range allNodes {
		nodeByName[n.Name] = n
	}
	for _, n := range eligible {
		nodeByName[n.Name] = n
	}

	// domainsWithMatch returns the set of topology domains holding a pod that
	// matches term t, plus whether any matching pod exists at all.
	domainsWithMatch := func(t corev1.PodAffinityTerm) (map[string]bool, bool) {
		matched := map[string]bool{}
		any := false
		for _, p := range placed {
			if !termMatches(pod, t, p.Namespace, p.Labels) {
				continue
			}
			any = true
			if n, ok := nodeByName[p.NodeName]; ok {
				if d, ok := nodeDomain(n, t.TopologyKey); ok {
					matched[d] = true
				}
			}
		}
		return matched, any
	}

	var aff []corev1.PodAffinityTerm
	var anti []corev1.PodAffinityTerm
	if pod.Spec.Affinity != nil {
		if pod.Spec.Affinity.PodAffinity != nil {
			aff = pod.Spec.Affinity.PodAffinity.RequiredDuringSchedulingIgnoredDuringExecution
		}
		if pod.Spec.Affinity.PodAntiAffinity != nil {
			anti = pod.Spec.Affinity.PodAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution
		}
	}

	if len(aff) > 0 {
		anyMatchAnywhere := false
		selfMatchesAll := true
		type termState struct {
			t       corev1.PodAffinityTerm
			matched map[string]bool
		}
		var states []termState
		for _, t := range aff {
			if _, _, ok := termNamespaces(pod, t); !ok {
				continue // namespaceSelector with labels: not modelled, don't guess
			}
			matched, any := domainsWithMatch(t)
			anyMatchAnywhere = anyMatchAnywhere || any
			if !termMatches(pod, t, pod.Namespace, pod.Labels) {
				selfMatchesAll = false
			}
			states = append(states, termState{t, matched})
		}
		selfException := !anyMatchAnywhere && selfMatchesAll
		if !selfException {
			for _, st := range states {
				ok := false
				for _, n := range eligible {
					d, has := nodeDomain(n, st.t.TopologyKey)
					if has && st.matched[d] {
						ok = true
						continue
					}
					setReason(reasons, n.Name, fmt.Sprintf("pod affinity: no matching pod in its %s domain", st.t.TopologyKey))
				}
				if !ok {
					causes = append(causes, Cause{
						Severity: Blocker,
						Title:    fmt.Sprintf("Required pod affinity unsatisfied (topology %q)", st.t.TopologyKey),
						Detail:   "The pod must be co-located (same topology domain) with pods matching its affinity selector, but no eligible node's domain currently contains such a pod.",
						Fix:      "Schedule a matching pod first, relax the affinity to preferred, or ensure nodes in the matching pod's domain are schedulable for this pod.",
					})
				}
			}
		}
	}

	for _, t := range anti {
		if _, _, ok := termNamespaces(pod, t); !ok {
			continue
		}
		matched, _ := domainsWithMatch(t)
		total, blocked := 0, 0
		for _, n := range eligible {
			d, has := nodeDomain(n, t.TopologyKey)
			if !has {
				continue // a node without the key can't conflict
			}
			total++
			if matched[d] {
				blocked++
				setReason(reasons, n.Name, fmt.Sprintf("pod anti-affinity: %s=%s already has a matching pod", t.TopologyKey, d))
			}
		}
		if total > 0 && blocked == total {
			causes = append(causes, Cause{
				Severity: Blocker,
				Title:    fmt.Sprintf("Required pod anti-affinity unsatisfied (topology %q)", t.TopologyKey),
				Detail:   fmt.Sprintf("The pod must avoid domains that already hold a matching pod, but all %d eligible %q domain(s) already contain one. This is the classic 'one replica per host' that runs out of hosts.", total, t.TopologyKey),
				Fix:      "Add a node in a fresh topology domain, or relax the anti-affinity to preferredDuringScheduling.",
			})
		}
	}

	// Symmetry: existing pods' required anti-affinity against this pod.
	blockedBy := map[string]string{} // node -> existing pod's term key
	for _, p := range placed {
		for _, t := range p.RequiredAntiAffinity {
			ns := map[string]bool{p.Namespace: true}
			all := false
			if t.NamespaceSelector != nil {
				if len(t.NamespaceSelector.MatchLabels) == 0 && len(t.NamespaceSelector.MatchExpressions) == 0 {
					all = true
				} else {
					continue
				}
			}
			for _, n := range t.Namespaces {
				ns[n] = true
			}
			if !all && !ns[pod.Namespace] {
				continue
			}
			if !LabelSelectorMatches(t.LabelSelector, pod.Labels) {
				continue
			}
			host, ok := nodeByName[p.NodeName]
			if !ok {
				continue
			}
			d, ok := nodeDomain(host, t.TopologyKey)
			if !ok {
				continue
			}
			for _, n := range eligible {
				if nd, ok := nodeDomain(n, t.TopologyKey); ok && nd == d {
					blockedBy[n.Name] = t.TopologyKey
					setReason(reasons, n.Name, fmt.Sprintf("existing pod %s/%s's anti-affinity excludes this pod from %s=%s", p.Namespace, p.Name, t.TopologyKey, d))
				}
			}
		}
	}
	if len(eligible) > 0 && len(blockedBy) == len(eligible) {
		causes = append(causes, Cause{
			Severity: Blocker,
			Title:    "Existing pods' anti-affinity rejects this pod on every eligible node",
			Detail:   "Pods already running have required podAntiAffinity whose selector matches this pod's labels, and every eligible node shares a topology domain with one of them. The scheduler enforces anti-affinity in both directions.",
			Fix:      "Relax the existing workload's anti-affinity, change this pod's labels if the match is accidental, or add a node in a domain those pods don't occupy.",
		})
	}
	return causes, reasons
}

// setReason records the first reason a node is rejected (later analyses don't
// overwrite an earlier, usually more fundamental, one).
func setReason(m map[string]string, node, reason string) {
	if _, ok := m[node]; !ok {
		m[node] = reason
	}
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
