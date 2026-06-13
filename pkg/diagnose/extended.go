package diagnose

import (
	"fmt"
	"sort"

	corev1 "k8s.io/api/core/v1"
)

// AnalyzeExtendedResources explains why a pod requesting extended resources
// (GPUs, hugepages, custom device-plugin resources) can't be placed: either no
// schedulable node advertises the resource at all (device plugin missing, no
// such hardware, or those nodes were filtered out), or no single node has enough
// of it free. Like CPU/memory, an extended request must be satisfied on ONE node.
func AnalyzeExtendedResources(pod *corev1.Pod, eligible []NodeView) []Cause {
	req := PodRequests(pod).Extended

	names := make([]string, 0, len(req))
	for n, q := range req {
		if q > 0 {
			names = append(names, n)
		}
	}
	sort.Strings(names)

	var causes []Cause
	for _, name := range names {
		need := req[name]
		advertisers := 0
		var maxFree, sumFree int64
		for _, nv := range eligible {
			capacity := Allocatable(nv.Node).Extended[name]
			if capacity <= 0 {
				continue
			}
			advertisers++
			free := capacity - nv.Used.Extended[name]
			if free > maxFree {
				maxFree = free
			}
			if free > 0 {
				sumFree += free
			}
		}

		switch {
		case advertisers == 0:
			causes = append(causes, Cause{
				Severity: Blocker,
				Title:    fmt.Sprintf("No eligible node provides %s", name),
				Detail: fmt.Sprintf(
					"The pod requests %d of %q, but no schedulable node advertises it. Common on-prem causes: the device plugin (e.g. the NVIDIA device plugin for nvidia.com/gpu) isn't installed or healthy, there's no node with that hardware, or the only nodes that have it were filtered out by a taint/selector/affinity — check the other findings.",
					need, name),
				Fix: fmt.Sprintf("Install/repair the device-plugin DaemonSet, add a node that advertises %s, or (if those nodes are tainted, as GPU nodes usually are) add the matching toleration.", name),
			})
		case need > maxFree:
			detail := fmt.Sprintf("Pod requests %d of %q. The most any single eligible node has free is %d", need, name, maxFree)
			if need <= sumFree {
				detail += fmt.Sprintf(" — there's %d free in aggregate, but a pod must fit on ONE node.", sumFree)
			} else {
				detail += fmt.Sprintf(", and only %d is free across all eligible nodes — not enough anywhere.", sumFree)
			}
			causes = append(causes, Cause{
				Severity: Blocker,
				Title:    fmt.Sprintf("Insufficient %s", name),
				Detail:   detail,
				Fix:      fmt.Sprintf("Free up %s (scale down pods holding it), add nodes that provide more of it, or lower the pod's request.", name),
			})
		}
	}
	return causes
}
