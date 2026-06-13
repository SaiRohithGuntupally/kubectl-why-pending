package diagnose

import (
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
)

// Severity ranks how strongly a cause blocks scheduling.
type Severity int

const (
	Blocker Severity = iota // almost certainly why it's stuck
	Warning                 // contributes, or filters out some nodes
	Info                    // context, not necessarily blocking
)

func (s Severity) Icon() string {
	switch s {
	case Blocker:
		return "✗"
	case Warning:
		return "!"
	default:
		return "ℹ"
	}
}

// Cause is a single human-readable finding with a suggested fix.
type Cause struct {
	Severity Severity
	Title    string
	Detail   string
	Fix      string
}

// NodeView pairs a node with the resources already requested by pods on it.
type NodeView struct {
	Node *corev1.Node
	Used Resources
}

// Input is everything Analyze needs — gathered once by the caller so the engine
// stays pure and testable without a live cluster.
type Input struct {
	Pod            *corev1.Pod
	Nodes          []NodeView
	SchedulerEvent string   // latest FailedScheduling message, if any
	UnboundPVCs    []string // referenced PVCs that are not Bound
}

// Result is the diagnosis for one pod.
type Result struct {
	Namespace      string
	PodName        string
	Request        Resources
	Causes         []Cause
	SchedulerEvent string
}

// Analyze runs a lightweight re-implementation of the scheduler's filtering to
// explain, in plain English, why a pod cannot be placed.
func Analyze(in Input) Result {
	req := PodRequests(in.Pod)
	res := Result{
		Namespace:      in.Pod.Namespace,
		PodName:        in.Pod.Name,
		Request:        req,
		SchedulerEvent: in.SchedulerEvent,
	}

	if len(in.UnboundPVCs) > 0 {
		res.Causes = append(res.Causes, Cause{
			Severity: Blocker,
			Title:    "Unbound PersistentVolumeClaim(s)",
			Detail:   fmt.Sprintf("The pod mounts PVC(s) that are not Bound: %s.", strings.Join(in.UnboundPVCs, ", ")),
			Fix:      "Check the PVC and its StorageClass. On-prem, this usually means no PersistentVolume satisfies the claim (no dynamic provisioner, or no matching static PV). Note: a WaitForFirstConsumer StorageClass intentionally stays Pending until scheduling — fix the other causes first.",
		})
	}

	// Classify every node by the first hard predicate it fails.
	counts := map[string]int{}
	taintSet := map[string]bool{}
	eligible := 0 // nodes passing taint/selector/affinity/cordon
	fit := 0      // eligible nodes that also have room
	var maxFreeCPU, maxFreeMem int64
	var sumFreeCPU, sumFreeMem int64
	var maxAllocCPU, maxAllocMem int64 // biggest single node, if it were empty

	for _, nv := range in.Nodes {
		node := nv.Node
		switch {
		case IsCordoned(node):
			counts["cordoned"]++
			continue
		case !IsReady(node):
			counts["notready"]++
			continue
		}
		if bad := UntoleratedTaints(in.Pod, node); len(bad) > 0 {
			counts["taint"]++
			for _, t := range bad {
				taintSet[fmt.Sprintf("%s=%s:%s", t.Key, t.Value, t.Effect)] = true
			}
			continue
		}
		if !MatchesNodeSelector(in.Pod, node) {
			counts["selector"]++
			continue
		}
		if !MatchesNodeAffinity(in.Pod, node) {
			counts["affinity"]++
			continue
		}

		eligible++
		alloc := Allocatable(node)
		if alloc.CPUMilli > maxAllocCPU {
			maxAllocCPU = alloc.CPUMilli
		}
		if alloc.MemBytes > maxAllocMem {
			maxAllocMem = alloc.MemBytes
		}
		freeCPU := alloc.CPUMilli - nv.Used.CPUMilli
		freeMem := alloc.MemBytes - nv.Used.MemBytes
		if freeCPU > maxFreeCPU {
			maxFreeCPU = freeCPU
		}
		if freeMem > maxFreeMem {
			maxFreeMem = freeMem
		}
		if freeCPU > 0 {
			sumFreeCPU += freeCPU
		}
		if freeMem > 0 {
			sumFreeMem += freeMem
		}
		cpuOK := freeCPU >= req.CPUMilli
		memOK := freeMem >= req.MemBytes
		if cpuOK && memOK {
			fit++
		} else {
			if !cpuOK {
				counts["insufficient-cpu"]++
			}
			if !memOK {
				counts["insufficient-mem"]++
			}
		}
	}

	total := len(in.Nodes)

	if total == 0 {
		res.Causes = append(res.Causes, Cause{
			Severity: Blocker,
			Title:    "No nodes in the cluster",
			Detail:   "The scheduler has no nodes to place pods on.",
			Fix:      "Join at least one worker node and ensure it reports Ready.",
		})
		sortCauses(res.Causes)
		return res
	}

	if n := counts["notready"]; n > 0 {
		res.Causes = append(res.Causes, Cause{
			Severity: Warning,
			Title:    fmt.Sprintf("%d/%d node(s) NotReady", n, total),
			Detail:   "NotReady nodes can't accept new pods (kubelet down, network/CNI not up, or disk/memory pressure).",
			Fix:      "kubectl get nodes; then `kubectl describe node <name>` and check the kubelet/CNI on the affected node.",
		})
	}

	if len(taintSet) > 0 {
		taints := keys(taintSet)
		res.Causes = append(res.Causes, Cause{
			Severity: severityIf(eligible == 0, Blocker, Warning),
			Title:    fmt.Sprintf("%d/%d node(s) have taints the pod doesn't tolerate", counts["taint"], total),
			Detail:   "Untolerated taints: " + strings.Join(taints, ", ") + ".\nOn-prem clusters commonly taint control-plane nodes (node-role.kubernetes.io/control-plane), so a small cluster can have nowhere left to schedule.",
			Fix:      "Add a matching toleration to the pod spec, or remove the taint with `kubectl taint nodes <node> <key>-` if the node should accept workloads.",
		})
	}

	if n := counts["selector"]; n > 0 {
		res.Causes = append(res.Causes, Cause{
			Severity: severityIf(eligible == 0, Blocker, Warning),
			Title:    fmt.Sprintf("%d/%d node(s) don't match the pod's nodeSelector", n, total),
			Detail:   "Required nodeSelector: " + formatMap(in.Pod.Spec.NodeSelector) + ".",
			Fix:      "Label a node to match (`kubectl label node <node> key=value`) or correct the pod's nodeSelector.",
		})
	}

	if n := counts["affinity"]; n > 0 {
		res.Causes = append(res.Causes, Cause{
			Severity: severityIf(eligible == 0, Blocker, Warning),
			Title:    fmt.Sprintf("%d/%d node(s) don't match required nodeAffinity", n, total),
			Detail:   "The pod's requiredDuringScheduling node affinity excludes these nodes.",
			Fix:      "Adjust the affinity rules or label nodes so at least one satisfies them.",
		})
	}

	if n := counts["cordoned"]; n > 0 {
		res.Causes = append(res.Causes, Cause{
			Severity: severityIf(eligible == 0 && len(taintSet) == 0, Blocker, Info),
			Title:    fmt.Sprintf("%d/%d node(s) are cordoned (SchedulingDisabled)", n, total),
			Detail:   "Cordoned nodes are intentionally excluded from scheduling.",
			Fix:      "If this was unintended: `kubectl uncordon <node>`.",
		})
	}

	// Resource analysis only matters when some nodes were otherwise eligible.
	if eligible > 0 && fit == 0 {
		fitsOnEmptyNode := req.CPUMilli <= maxAllocCPU && req.MemBytes <= maxAllocMem
		fitsByTotalFree := req.CPUMilli <= sumFreeCPU && req.MemBytes <= sumFreeMem
		fitsOnLargestFree := req.CPUMilli <= maxFreeCPU && req.MemBytes <= maxFreeMem
		switch {
		case !fitsOnEmptyNode:
			// No single node is big enough even when completely empty —
			// rebalancing can't help; the pod or the hardware is the problem.
			res.Causes = append(res.Causes, Cause{
				Severity: Blocker,
				Title:    "Pod is larger than any single node",
				Detail: fmt.Sprintf(
					"Pod requests %s CPU / %s memory, but the biggest eligible node only has %s CPU / %s memory allocatable even when empty. A pod must fit on ONE node, so no amount of freeing-up will help.",
					FormatCPU(req.CPUMilli), FormatMem(req.MemBytes),
					FormatCPU(maxAllocCPU), FormatMem(maxAllocMem)),
				Fix: "Reduce the pod's resource requests, split the workload, or add a node large enough to hold it.",
			})
		case fitsByTotalFree && !fitsOnLargestFree:
			res.Causes = append(res.Causes, Cause{
				Severity: Blocker,
				Title:    "Resource fragmentation — capacity exists, but not on any single node",
				Detail: fmt.Sprintf(
					"Pod requests %s CPU / %s memory.\nTotal free across %d eligible node(s): %s CPU / %s memory — enough in aggregate.\nBut the largest single node only has %s CPU / %s memory free, and a pod must fit on ONE node.",
					FormatCPU(req.CPUMilli), FormatMem(req.MemBytes),
					eligible, FormatCPU(sumFreeCPU), FormatMem(sumFreeMem),
					FormatCPU(maxFreeCPU), FormatMem(maxFreeMem)),
				Fix: "Defragment the cluster: drain/rebalance pods to free a contiguous slot, lower this pod's requests if they're padded, or add a node large enough to hold it. (In the cloud the autoscaler would add a node here — on bare metal that's on you.)",
			})
		default:
			res.Causes = append(res.Causes, Cause{
				Severity: Blocker,
				Title:    "Insufficient free capacity right now",
				Detail: fmt.Sprintf(
					"Pod requests %s CPU / %s memory. No eligible node has room, and the combined free capacity (%s CPU / %s memory across %d node(s)) isn't enough either.",
					FormatCPU(req.CPUMilli), FormatMem(req.MemBytes),
					FormatCPU(sumFreeCPU), FormatMem(sumFreeMem), eligible),
				Fix: "Free up capacity (scale down or evict lower-priority pods), add worker nodes, or reduce the pod's resource requests.",
			})
		}
	}

	// Honest about the limits of a static analysis.
	if fit > 0 && len(res.Causes) == 0 {
		res.Causes = append(res.Causes, Cause{
			Severity: Info,
			Title:    fmt.Sprintf("%d node(s) look like they should fit", fit),
			Detail:   "Static analysis didn't find a hard blocker. The cause is likely dynamic: pod priority/preemption, pod topology spread or pod (anti-)affinity, extended/custom resources (GPUs), or simply a very recent scheduling attempt.",
			Fix:      "Read the scheduler event below, and check `kubectl describe pod` for topology-spread or pod-affinity constraints.",
		})
	}

	sortCauses(res.Causes)
	return res
}

func sortCauses(c []Cause) {
	sort.SliceStable(c, func(i, j int) bool { return c[i].Severity < c[j].Severity })
}

func severityIf(cond bool, a, b Severity) Severity {
	if cond {
		return a
	}
	return b
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func formatMap(m map[string]string) string {
	if len(m) == 0 {
		return "(none)"
	}
	parts := make([]string, 0, len(m))
	for k, v := range m {
		parts = append(parts, k+"="+v)
	}
	sort.Strings(parts)
	return strings.Join(parts, ", ")
}
