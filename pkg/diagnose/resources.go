package diagnose

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
)

// Resources is a coarse CPU (millicores) + memory (bytes) pair, which is all
// the scheduler's resource-fit check cares about for the common cases.
type Resources struct {
	CPUMilli int64
	MemBytes int64
}

// PodRequests returns the effective resource request for a pod: the larger of
// (sum of normal container requests) and (max of any single init container),
// which matches how the scheduler reserves space.
func PodRequests(pod *corev1.Pod) Resources {
	var sum Resources
	for i := range pod.Spec.Containers {
		req := pod.Spec.Containers[i].Resources.Requests
		sum.CPUMilli += req.Cpu().MilliValue()
		sum.MemBytes += req.Memory().Value()
	}
	var initMax Resources
	for i := range pod.Spec.InitContainers {
		req := pod.Spec.InitContainers[i].Resources.Requests
		if v := req.Cpu().MilliValue(); v > initMax.CPUMilli {
			initMax.CPUMilli = v
		}
		if v := req.Memory().Value(); v > initMax.MemBytes {
			initMax.MemBytes = v
		}
	}
	if initMax.CPUMilli > sum.CPUMilli {
		sum.CPUMilli = initMax.CPUMilli
	}
	if initMax.MemBytes > sum.MemBytes {
		sum.MemBytes = initMax.MemBytes
	}
	return sum
}

// Allocatable is the schedulable capacity a node advertises.
func Allocatable(node *corev1.Node) Resources {
	return Resources{
		CPUMilli: node.Status.Allocatable.Cpu().MilliValue(),
		MemBytes: node.Status.Allocatable.Memory().Value(),
	}
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
