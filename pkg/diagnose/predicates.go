package diagnose

import (
	"strconv"

	corev1 "k8s.io/api/core/v1"
)

// IsCordoned reports whether a node has been marked unschedulable.
func IsCordoned(node *corev1.Node) bool {
	return node.Spec.Unschedulable
}

// IsReady reports whether the node's Ready condition is True.
func IsReady(node *corev1.Node) bool {
	for _, c := range node.Status.Conditions {
		if c.Type == corev1.NodeReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// UntoleratedTaints returns the node taints (with a scheduling effect) that the
// pod does not tolerate. Empty means the pod tolerates everything on this node.
func UntoleratedTaints(pod *corev1.Pod, node *corev1.Node) []corev1.Taint {
	var bad []corev1.Taint
	for i := range node.Spec.Taints {
		taint := node.Spec.Taints[i]
		if taint.Effect != corev1.TaintEffectNoSchedule && taint.Effect != corev1.TaintEffectNoExecute {
			continue // PreferNoSchedule never blocks scheduling
		}
		tolerated := false
		for j := range pod.Spec.Tolerations {
			if tolerationTolerates(pod.Spec.Tolerations[j], taint) {
				tolerated = true
				break
			}
		}
		if !tolerated {
			bad = append(bad, taint)
		}
	}
	return bad
}

// tolerationTolerates reports whether a single toleration covers a taint,
// implementing the Kubernetes match rules (Equal is the default operator; an
// empty key with Exists matches every key; an empty effect matches all effects).
func tolerationTolerates(t corev1.Toleration, taint corev1.Taint) bool {
	if t.Effect != "" && t.Effect != taint.Effect {
		return false
	}
	if t.Key == "" {
		return t.Operator == corev1.TolerationOpExists
	}
	if t.Key != taint.Key {
		return false
	}
	op := t.Operator
	if op == "" {
		op = corev1.TolerationOpEqual
	}
	switch op {
	case corev1.TolerationOpExists:
		return true
	case corev1.TolerationOpEqual:
		return t.Value == taint.Value
	}
	return false
}

// MatchesNodeSelector reports whether the node satisfies the pod's simple
// nodeSelector (a label subset match).
func MatchesNodeSelector(pod *corev1.Pod, node *corev1.Node) bool {
	for k, v := range pod.Spec.NodeSelector {
		if node.Labels[k] != v {
			return false
		}
	}
	return true
}

// MatchesNodeAffinity reports whether the node satisfies the pod's required
// (hard) node affinity. Soft (preferred) affinity never blocks scheduling and
// is ignored here.
func MatchesNodeAffinity(pod *corev1.Pod, node *corev1.Node) bool {
	if pod.Spec.Affinity == nil || pod.Spec.Affinity.NodeAffinity == nil {
		return true
	}
	req := pod.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution
	if req == nil || len(req.NodeSelectorTerms) == 0 {
		return true
	}
	for i := range req.NodeSelectorTerms {
		if matchTerm(req.NodeSelectorTerms[i], node) {
			return true // terms are OR'd together
		}
	}
	return false
}

func matchTerm(term corev1.NodeSelectorTerm, node *corev1.Node) bool {
	for i := range term.MatchExpressions {
		if !matchExpr(term.MatchExpressions[i], node.Labels) {
			return false
		}
	}
	for i := range term.MatchFields {
		e := term.MatchFields[i]
		val := ""
		if e.Key == "metadata.name" {
			val = node.Name
		}
		if !matchExprValue(e, val, true) {
			return false
		}
	}
	return true
}

func matchExpr(e corev1.NodeSelectorRequirement, labels map[string]string) bool {
	val, ok := labels[e.Key]
	return matchExprValue(e, val, ok)
}

func matchExprValue(e corev1.NodeSelectorRequirement, val string, present bool) bool {
	switch e.Operator {
	case corev1.NodeSelectorOpIn:
		return present && contains(e.Values, val)
	case corev1.NodeSelectorOpNotIn:
		return !present || !contains(e.Values, val)
	case corev1.NodeSelectorOpExists:
		return present
	case corev1.NodeSelectorOpDoesNotExist:
		return !present
	case corev1.NodeSelectorOpGt, corev1.NodeSelectorOpLt:
		if !present || len(e.Values) == 0 {
			return false
		}
		lv, err1 := strconv.ParseInt(val, 10, 64)
		rv, err2 := strconv.ParseInt(e.Values[0], 10, 64)
		if err1 != nil || err2 != nil {
			return false
		}
		if e.Operator == corev1.NodeSelectorOpGt {
			return lv > rv
		}
		return lv < rv
	}
	return false
}

func contains(values []string, v string) bool {
	for _, x := range values {
		if x == v {
			return true
		}
	}
	return false
}
