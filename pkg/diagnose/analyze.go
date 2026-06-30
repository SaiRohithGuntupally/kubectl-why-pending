package diagnose

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	resourcev1 "k8s.io/api/resource/v1"
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

// String renders the severity for structured output.
func (s Severity) String() string {
	switch s {
	case Blocker:
		return "blocker"
	case Warning:
		return "warning"
	default:
		return "info"
	}
}

// MarshalJSON emits the severity as its name ("blocker"/"warning"/"info")
// rather than an opaque integer.
func (s Severity) MarshalJSON() ([]byte, error) {
	return []byte(strconv.Quote(s.String())), nil
}

// Cause is a single human-readable finding with a suggested fix.
type Cause struct {
	Severity Severity `json:"severity"`
	Title    string   `json:"title"`
	Detail   string   `json:"detail"`
	Fix      string   `json:"fix"`
}

// NodeView pairs a node with the resources already requested by pods on it.
type NodeView struct {
	Node *corev1.Node
	Used Resources
}

// NodeVerdict is the per-node breakdown — the "nodes evaluated, and why each was
// filtered" view that consumers (and kubernetes/kubernetes#53908) ask for.
type NodeVerdict struct {
	Name        string `json:"name"`
	Schedulable bool   `json:"schedulable"`
	Reason      string `json:"reason,omitempty"`
}

// Input is everything Analyze needs — gathered once by the caller so the engine
// stays pure and testable without a live cluster.
type Input struct {
	Pod            *corev1.Pod
	Nodes          []NodeView
	ClusterPods    []PlacedPod  // all placed pods, for topology/affinity analysis
	SchedulerEvent string       // latest FailedScheduling message, if any
	UnboundPVCs    []string     // referenced PVCs that exist but are not Bound
	MissingPVCs    []string     // referenced PVCs that don't exist at all
	Chain          *ChainStatus // GPU enablement-chain status (nil if not analyzed)

	// DRA (Dynamic Resource Allocation, k8s 1.34+) inputs, populated only when a
	// pending pod uses pod.spec.resourceClaims. Claims may span namespaces;
	// slices and classes are cluster-scoped.
	DRAClaims  []resourcev1.ResourceClaim
	DRASlices  []resourcev1.ResourceSlice
	DRAClasses []resourcev1.DeviceClass
}

// Result is the diagnosis for one pod.
type Result struct {
	Namespace      string        `json:"namespace"`
	PodName        string        `json:"pod"`
	Request        Resources     `json:"request"`
	Causes         []Cause       `json:"causes"`
	Nodes          []NodeVerdict `json:"nodes,omitempty"`
	SchedulerEvent string        `json:"schedulerEvent,omitempty"`
}

// HasBlocker reports whether any cause is a Blocker (used for the CLI exit code).
func (r Result) HasBlocker() bool {
	for _, c := range r.Causes {
		if c.Severity == Blocker {
			return true
		}
	}
	return false
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

	if len(in.MissingPVCs) > 0 {
		res.Causes = append(res.Causes, Cause{
			Severity: Blocker,
			Title:    "PersistentVolumeClaim(s) not found",
			Detail:   fmt.Sprintf("The pod references PVC(s) that do not exist: %s.", strings.Join(in.MissingPVCs, ", ")),
			Fix:      "Create the missing PVC(s), or correct the volume claim name in the pod spec.",
		})
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
	var eligibleViews []NodeView
	var maxFreeCPU, maxFreeMem int64
	var sumFreeCPU, sumFreeMem int64
	var maxAllocCPU, maxAllocMem int64 // biggest single node, if it were empty

	for _, nv := range in.Nodes {
		node := nv.Node
		switch {
		case IsCordoned(node):
			counts["cordoned"]++
			res.Nodes = append(res.Nodes, NodeVerdict{node.Name, false, "cordoned (SchedulingDisabled)"})
			continue
		case !IsReady(node):
			counts["notready"]++
			res.Nodes = append(res.Nodes, NodeVerdict{node.Name, false, "NotReady"})
			continue
		}
		if bad := UntoleratedTaints(in.Pod, node); len(bad) > 0 {
			counts["taint"]++
			keys := make([]string, 0, len(bad))
			for _, t := range bad {
				taintSet[fmt.Sprintf("%s=%s:%s", t.Key, t.Value, t.Effect)] = true
				keys = append(keys, t.Key)
			}
			res.Nodes = append(res.Nodes, NodeVerdict{node.Name, false, "untolerated taint: " + strings.Join(keys, ",")})
			continue
		}
		if !MatchesNodeSelector(in.Pod, node) {
			counts["selector"]++
			res.Nodes = append(res.Nodes, NodeVerdict{node.Name, false, "nodeSelector mismatch"})
			continue
		}
		if !MatchesNodeAffinity(in.Pod, node) {
			counts["affinity"]++
			res.Nodes = append(res.Nodes, NodeVerdict{node.Name, false, "nodeAffinity mismatch"})
			continue
		}

		eligible++
		eligibleViews = append(eligibleViews, nv)
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
			res.Nodes = append(res.Nodes, NodeVerdict{node.Name, true, "fits (CPU/memory)"})
		} else {
			var lack []string
			if !cpuOK {
				counts["insufficient-cpu"]++
				lack = append(lack, "CPU")
			}
			if !memOK {
				counts["insufficient-mem"]++
				lack = append(lack, "memory")
			}
			res.Nodes = append(res.Nodes, NodeVerdict{node.Name, false, "insufficient " + strings.Join(lack, "+")})
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
			Severity: severityIf(eligible == 0, Blocker, Warning),
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

	// Extended resources (GPUs, hugepages, custom device-plugin resources).
	res.Causes = append(res.Causes, AnalyzeExtendedResources(in.Pod, eligibleViews, in.Chain)...)

	// Cross-pod constraints: topology spread and inter-pod (anti-)affinity.
	allNodes := make([]*corev1.Node, 0, len(in.Nodes))
	for _, nv := range in.Nodes {
		allNodes = append(allNodes, nv.Node)
	}
	eligibleNodes := make([]*corev1.Node, 0, len(eligibleViews))
	for _, nv := range eligibleViews {
		eligibleNodes = append(eligibleNodes, nv.Node)
	}
	res.Causes = append(res.Causes, AnalyzeTopologySpread(in.Pod, allNodes, eligibleNodes, in.ClusterPods)...)
	res.Causes = append(res.Causes, AnalyzePodAffinity(in.Pod, eligibleNodes, in.ClusterPods)...)

	// Dynamic Resource Allocation: pods requesting devices via resourceClaims are
	// invisible to the extended-resource path, so diagnose them separately.
	if UsesDRA(in.Pod) {
		res.Causes = append(res.Causes, AnalyzeDRA(in.Pod, in.DRAClaims, in.DRASlices, in.DRAClasses)...)
	}

	// Honest about the limits of a static analysis. Whenever no specific cause
	// was found, say so explicitly rather than emitting an empty diagnosis —
	// the wording differs depending on whether any node looked schedulable.
	if len(res.Causes) == 0 {
		if fit > 0 {
			res.Causes = append(res.Causes, Cause{
				Severity: Info,
				Title:    fmt.Sprintf("%d node(s) look like they should fit", fit),
				Detail:   "Static analysis didn't find a hard blocker. The cause is likely dynamic: pod priority/preemption, pod topology spread or pod (anti-)affinity, extended/custom resources (GPUs), or simply a very recent scheduling attempt.",
				Fix:      "Read the scheduler event below, and check `kubectl describe pod` for topology-spread or pod-affinity constraints.",
			})
		} else {
			// Defensive: every fit==0 path above should already have recorded a
			// cause, so reaching here means a blocker we don't model yet. Stay
			// honest instead of printing "No blocking cause found".
			res.Causes = append(res.Causes, Cause{
				Severity: Info,
				Title:    "Could not determine a static cause",
				Detail:   "No eligible node could host this pod, but none of the causes this tool models (capacity, taints, selectors/affinity, extended resources, topology spread, pod affinity, PVCs, node health) matched. The reason is likely dynamic or not yet modeled here.",
				Fix:      "Read the scheduler event below, and run `kubectl describe pod` / `kubectl get events` for the scheduler's own explanation.",
			})
		}
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
