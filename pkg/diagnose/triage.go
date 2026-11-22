package diagnose

import (
	"fmt"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
)

// triage catches the Pending pods that are NOT waiting on a scheduler filter,
// before any capacity math runs. Running predicates on these produces confident
// but fabricated blockers (audit risks 1 and 20), so Analyze returns early when
// triage reports done=true.
func triage(in Input) (causes []Cause, done bool) {
	pod := in.Pod
	// Bound to a node that isn't in the cluster: set by hand (spec.nodeName
	// bypasses the scheduler) or the node was deleted. Nothing will run it.
	if pod.Spec.NodeName != "" && len(in.Nodes) > 0 && !nodeExists(in.Nodes, pod.Spec.NodeName) {
		return []Cause{{
			Severity: Blocker,
			Title:    fmt.Sprintf("Bound to node %s, which doesn't exist", pod.Spec.NodeName),
			Detail:   fmt.Sprintf("spec.nodeName is %q but no such node is in the cluster. Setting nodeName skips the scheduler entirely, so nothing will move the pod; the pod garbage collector deletes pods bound to missing nodes.", pod.Spec.NodeName),
			Fix:      "Remove spec.nodeName (use a nodeSelector on kubernetes.io/hostname instead) or fix the node name, then recreate the pod.",
		}}, true
	}
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

	// No scheduler has tried this pod. With a custom schedulerName, that
	// scheduler is likely not running (or doesn't watch this namespace); with
	// the default one, kube-scheduler may be down or stuck on leader election.
	since := pod.CreationTimestamp.Time
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodScheduled && c.LastTransitionTime.After(since) {
			since = c.LastTransitionTime.Time // e.g. scheduling gates just lifted
		}
	}
	if !in.SchedulerEventSeen && !in.Now.IsZero() && in.Now.Sub(since) > time.Minute {
		name := pod.Spec.SchedulerName
		if name == "" {
			name = corev1.DefaultSchedulerName
		}
		age := in.Now.Sub(since).Round(time.Second)
		if name != corev1.DefaultSchedulerName {
			return []Cause{{
				Severity: Blocker,
				Title:    fmt.Sprintf("Scheduler %q has never tried this pod", name),
				Detail:   fmt.Sprintf("The pod sets schedulerName: %s and has no scheduling events after %s. The default scheduler ignores pods that name another scheduler, so if %q isn't running (or doesn't handle this namespace) the pod waits forever. The checks this tool runs model the default scheduler only.", name, age, name),
				Fix:      fmt.Sprintf("Check that the %q scheduler is deployed and healthy, or remove schedulerName to use the default scheduler.", name),
			}}, true
		}
		causes = append(causes, Cause{
			Severity: Warning,
			Title:    "No scheduling events yet",
			Detail:   fmt.Sprintf("The pod is %s old and kube-scheduler hasn't recorded a FailedScheduling event for it. Either it just retried successfully, or the scheduler isn't running (or is stuck on leader election).", age),
			Fix:      "Check `kubectl -n kube-system get pods -l component=kube-scheduler` and `kubectl -n kube-system get lease kube-scheduler`.",
		})
	}

	// Nominated node: the scheduler has a plan (usually preemption of
	// lower-priority pods in progress, or a PreBind wait). Keep analysing, but
	// Analyze downgrades blockers because the scheduler is already acting.
	if pod.Status.NominatedNodeName != "" {
		return append(causes, Cause{
			Severity: Info,
			Title:    fmt.Sprintf("Scheduler nominated node %s (preemption or binding in progress)", pod.Status.NominatedNodeName),
			Detail:   "status.nominatedNodeName is set: the scheduler found a node, typically after choosing lower-priority pods to evict, and is waiting for them to terminate. The checks below show the state before that finishes.",
			Fix:      "Usually nothing: wait for the preempted pods' graceful termination. If it stays this way, look for PodDisruptionBudgets or long terminationGracePeriodSeconds on the victims.",
		}), false
	}
	return causes, false
}

func nodeExists(nodes []NodeView, name string) bool {
	for _, nv := range nodes {
		if nv.Node.Name == name {
			return true
		}
	}
	return false
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
