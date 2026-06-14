package diagnose

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
)

// Resources is the scheduler-relevant resource view: CPU (millicores), memory
// (bytes), and any extended resources (e.g. nvidia.com/gpu, hugepages-2Mi) keyed
// by name. Extended quantities are whole-number counts (or bytes for hugepages).
type Resources struct {
	CPUMilli int64            `json:"cpuMilli"`
	MemBytes int64            `json:"memBytes"`
	Extended map[string]int64 `json:"extended,omitempty"`
}

// isStandardResource reports whether a resource is one the scheduler accounts for
// specially (CPU/memory/ephemeral-storage); everything else is "extended".
func isStandardResource(name corev1.ResourceName) bool {
	switch name {
	case corev1.ResourceCPU, corev1.ResourceMemory, corev1.ResourceEphemeralStorage:
		return true
	}
	return false
}

// PodRequests returns the effective resource request for a pod: the larger of
// (sum of normal container requests) and (max of any single init container),
// which matches how the scheduler reserves space.
func PodRequests(pod *corev1.Pod) Resources {
	sum := Resources{Extended: map[string]int64{}}
	for i := range pod.Spec.Containers {
		req := pod.Spec.Containers[i].Resources.Requests
		sum.CPUMilli += req.Cpu().MilliValue()
		sum.MemBytes += req.Memory().Value()
		for name, q := range req {
			if !isStandardResource(name) {
				sum.Extended[string(name)] += q.Value()
			}
		}
	}
	initMax := Resources{Extended: map[string]int64{}}
	for i := range pod.Spec.InitContainers {
		req := pod.Spec.InitContainers[i].Resources.Requests
		if v := req.Cpu().MilliValue(); v > initMax.CPUMilli {
			initMax.CPUMilli = v
		}
		if v := req.Memory().Value(); v > initMax.MemBytes {
			initMax.MemBytes = v
		}
		for name, q := range req {
			if !isStandardResource(name) {
				if v := q.Value(); v > initMax.Extended[string(name)] {
					initMax.Extended[string(name)] = v
				}
			}
		}
	}
	if initMax.CPUMilli > sum.CPUMilli {
		sum.CPUMilli = initMax.CPUMilli
	}
	if initMax.MemBytes > sum.MemBytes {
		sum.MemBytes = initMax.MemBytes
	}
	for name, v := range initMax.Extended {
		if v > sum.Extended[name] {
			sum.Extended[name] = v
		}
	}
	return sum
}

// Allocatable is the schedulable capacity a node advertises, including any
// extended resources (GPUs, hugepages, custom device-plugin resources).
func Allocatable(node *corev1.Node) Resources {
	r := Resources{
		CPUMilli: node.Status.Allocatable.Cpu().MilliValue(),
		MemBytes: node.Status.Allocatable.Memory().Value(),
		Extended: map[string]int64{},
	}
	for name, q := range node.Status.Allocatable {
		if !isStandardResource(name) {
			r.Extended[string(name)] = q.Value()
		}
	}
	return r
}

// FormatCPU renders millicores as a human string ("1500m" or "2").
func FormatCPU(milli int64) string {
	if milli%1000 == 0 {
		return fmt.Sprintf("%d", milli/1000)
	}
	return fmt.Sprintf("%dm", milli)
}

// FormatMem renders bytes as the nearest binary unit (Mi/Gi).
func FormatMem(b int64) string {
	const Mi = 1024 * 1024
	const Gi = 1024 * Mi
	switch {
	case b >= Gi:
		return fmt.Sprintf("%.1fGi", float64(b)/float64(Gi))
	case b >= Mi:
		return fmt.Sprintf("%.0fMi", float64(b)/float64(Mi))
	default:
		return fmt.Sprintf("%dB", b)
	}
}
