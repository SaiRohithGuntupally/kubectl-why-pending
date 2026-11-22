package diagnose

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
)

// Resources is the scheduler-relevant resource view: CPU (millicores), memory
// and ephemeral-storage (bytes), and any extended resources (e.g.
// nvidia.com/gpu, hugepages-2Mi) keyed by name. Pods is only meaningful for a
// node's usage/allocatable (how many pods it runs vs its max-pods).
type Resources struct {
	CPUMilli       int64            `json:"cpuMilli"`
	MemBytes       int64            `json:"memBytes"`
	EphemeralBytes int64            `json:"ephemeralBytes,omitempty"`
	Pods           int64            `json:"-"`
	Extended       map[string]int64 `json:"extended,omitempty"`
}

// isStandardResource reports whether a resource is one the scheduler accounts for
// specially (CPU/memory/ephemeral-storage/pods); everything else is "extended".
func isStandardResource(name corev1.ResourceName) bool {
	switch name {
	case corev1.ResourceCPU, corev1.ResourceMemory, corev1.ResourceEphemeralStorage, corev1.ResourcePods:
		return true
	}
	return false
}

func (r *Resources) add(l corev1.ResourceList) {
	r.CPUMilli += l.Cpu().MilliValue()
	r.MemBytes += l.Memory().Value()
	r.EphemeralBytes += l.StorageEphemeral().Value()
	for name, q := range l {
		if !isStandardResource(name) {
			r.Extended[string(name)] += q.Value()
		}
	}
}

func (r *Resources) maxWith(o Resources) {
	r.CPUMilli = max(r.CPUMilli, o.CPUMilli)
	r.MemBytes = max(r.MemBytes, o.MemBytes)
	r.EphemeralBytes = max(r.EphemeralBytes, o.EphemeralBytes)
	for name, v := range o.Extended {
		r.Extended[name] = max(r.Extended[name], v)
	}
}

func (r Resources) clone() Resources {
	c := r
	c.Extended = make(map[string]int64, len(r.Extended))
	for k, v := range r.Extended {
		c.Extended[k] = v
	}
	return c
}

func newResources() Resources { return Resources{Extended: map[string]int64{}} }

// PodRequests returns the effective request the scheduler reserves for a pod,
// following the upstream resource helper:
//   - regular containers are summed;
//   - init containers run one at a time, so each contributes its own request
//     plus every sidecar (init container with restartPolicy: Always) started
//     before it, and the pod needs the max of those;
//   - sidecars keep running, so they are also added to the regular total;
//   - spec.overhead (RuntimeClass, e.g. Kata/gVisor) is added on top.
//   - a container's request is the larger of its spec and the resources the
//     kubelet allocated (in-place resize, GA 1.35: while a resize is pending the
//     scheduler reserves the larger of the two);
//   - pod-level spec.resources (beta, on by default since 1.34), when set,
//     replaces the container sum for cpu and memory.
func PodRequests(pod *corev1.Pod) Resources {
	allocated := map[string]corev1.ResourceList{}
	for _, cs := range pod.Status.ContainerStatuses {
		allocated[cs.Name] = cs.AllocatedResources
	}
	for _, cs := range pod.Status.InitContainerStatuses {
		allocated[cs.Name] = cs.AllocatedResources
	}
	req := func(c *corev1.Container) corev1.ResourceList {
		a := allocated[c.Name]
		if len(a) == 0 {
			return c.Resources.Requests
		}
		out := corev1.ResourceList{}
		for k, v := range c.Resources.Requests {
			out[k] = v
		}
		for k, v := range a {
			if cur, ok := out[k]; !ok || v.Cmp(cur) > 0 {
				out[k] = v
			}
		}
		return out
	}

	sum := newResources()
	for i := range pod.Spec.Containers {
		sum.add(req(&pod.Spec.Containers[i]))
	}

	sidecars := newResources()
	initPeak := newResources()
	for i := range pod.Spec.InitContainers {
		c := &pod.Spec.InitContainers[i]
		if c.RestartPolicy != nil && *c.RestartPolicy == corev1.ContainerRestartPolicyAlways {
			sidecars.add(req(c))
			initPeak.maxWith(sidecars) // the sidecar running alongside earlier ones
			continue
		}
		step := sidecars.clone()
		step.add(req(c))
		initPeak.maxWith(step)
	}

	sum.CPUMilli += sidecars.CPUMilli
	sum.MemBytes += sidecars.MemBytes
	sum.EphemeralBytes += sidecars.EphemeralBytes
	for name, v := range sidecars.Extended {
		sum.Extended[name] += v
	}
	sum.maxWith(initPeak)

	if pr := pod.Spec.Resources; pr != nil {
		if q, ok := pr.Requests[corev1.ResourceCPU]; ok {
			sum.CPUMilli = q.MilliValue()
		}
		if q, ok := pr.Requests[corev1.ResourceMemory]; ok {
			sum.MemBytes = q.Value()
		}
	}

	if pod.Spec.Overhead != nil {
		sum.add(pod.Spec.Overhead)
	}
	return sum
}

// Allocatable is the schedulable capacity a node advertises, including any
// extended resources (GPUs, hugepages, custom device-plugin resources).
func Allocatable(node *corev1.Node) Resources {
	r := Resources{
		CPUMilli:       node.Status.Allocatable.Cpu().MilliValue(),
		MemBytes:       node.Status.Allocatable.Memory().Value(),
		EphemeralBytes: node.Status.Allocatable.StorageEphemeral().Value(),
		Pods:           node.Status.Allocatable.Pods().Value(),
		Extended:       map[string]int64{},
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
