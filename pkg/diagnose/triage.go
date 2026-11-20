package diagnose

import (
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
)

// triage catches the Pending pods that are NOT waiting on a scheduler filter,
// before any capacity math runs. Running predicates on these produces confident
// but fabricated blockers (audit risks 1 and 20), so Analyze returns early when
// triage reports done=true.
func triage(pod *corev1.Pod) (causes []Cause, done bool) {
	// Already bound: the scheduler finished its job. Pending now means the
	// kubelet/runtime side (image pull, volume mount, sandbox, init containers).
	if pod.Spec.NodeName != "" {
		detail := fmt.Sprintf("The scheduler already placed this pod on node %q. It shows Pending because the kubelet hasn't started all containers yet.", pod.Spec.NodeName)
		if w := waitingReasons(pod); w != "" {
			detail += "\nContainer state: " + w + "."
		}
		return []Cause{{
			Severity: Warning,
			Title:    fmt.Sprintf("Already scheduled to %s; stuck on the node, not the scheduler", pod.Spec.NodeName),
			Detail:   detail,
			Fix:      fmt.Sprintf("`kubectl describe pod -n %s %s` and read the Events (image pull, FailedMount, FailedCreatePodSandBox). This tool diagnoses scheduling, not kubelet start-up.", pod.Namespace, pod.Name),
		}}, true
	}

	// Scheduling gates: the scheduler will not even look at the pod until every
	// gate is removed by whatever controller added it (Kueue, Volcano, custom).
	if len(pod.Spec.SchedulingGates) > 0 {
		names := make([]string, 0, len(pod.Spec.SchedulingGates))
		for _, g := range pod.Spec.SchedulingGates {
			names = append(names, g.Name)
		}
		detail := "The pod has scheduling gates: " + strings.Join(names, ", ") + ". The scheduler skips gated pods entirely, so no capacity, taint or affinity check applies yet."
		if m := gateManagers(pod); m != "" {
			detail += "\nGate(s) set by: " + m + "."
		}
		return []Cause{{
			Severity: Info,
			Title:    "Held by a scheduling gate; the scheduler hasn't tried this pod yet",
			Detail:   detail,
			Fix:      "Check the controller that owns the gate (for Kueue: `kubectl get workloads -n " + pod.Namespace + "` and the ClusterQueue quota). The gate is removed when that controller admits the pod.",
		}}, true
	}

	// Nominated node: the scheduler has a plan (usually preemption of
	// lower-priority pods in progress, or a PreBind wait). Keep analysing, but
	// Analyze downgrades blockers because the scheduler is already acting.
	if pod.Status.NominatedNodeName != "" {
		return []Cause{{
			Severity: Info,
			Title:    fmt.Sprintf("Scheduler nominated node %s (preemption or binding in progress)", pod.Status.NominatedNodeName),
			Detail:   "status.nominatedNodeName is set: the scheduler found a node, typically after choosing lower-priority pods to evict, and is waiting for them to terminate. The checks below show the state before that finishes.",
			Fix:      "Usually nothing: wait for the preempted pods' graceful termination. If it stays this way, look for PodDisruptionBudgets or long terminationGracePeriodSeconds on the victims.",
		}}, false
	}
	return nil, false
}

// waitingReasons summarises why containers haven't started, e.g.
// "c: ImagePullBackOff (Back-off pulling image ...)".
func waitingReasons(pod *corev1.Pod) string {
	var out []string
	add := func(statuses []corev1.ContainerStatus) {
		for _, cs := range statuses {
			if w := cs.State.Waiting; w != nil && w.Reason != "" {
				s := cs.Name + ": " + w.Reason
				if w.Message != "" {
					msg := w.Message
					if len(msg) > 120 {
						msg = msg[:120] + "..."
					}
					s += " (" + msg + ")"
				}
				out = append(out, s)
			}
		}
	}
	add(pod.Status.InitContainerStatuses)
	add(pod.Status.ContainerStatuses)
	return strings.Join(out, "; ")
}

// gateManagers returns the field managers that own spec.schedulingGates, read
// from managedFields (e.g. "kueue-admission").
func gateManagers(pod *corev1.Pod) string {
	set := map[string]bool{}
	for _, mf := range pod.ManagedFields {
		if mf.FieldsV1 != nil && strings.Contains(string(mf.FieldsV1.Raw), "f:schedulingGates") {
			set[mf.Manager] = true
		}
	}
	names := make([]string, 0, len(set))
	for m := range set {
		names = append(names, m)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}
