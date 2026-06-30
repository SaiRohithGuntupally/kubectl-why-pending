package diagnose

import (
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	resourcev1 "k8s.io/api/resource/v1"
)

// UsesDRA reports whether the pod requests devices via Dynamic Resource
// Allocation (k8s 1.34+) — through pod.spec.resourceClaims rather than
// nvidia.com/gpu-style extended resources. Such pods are invisible to the
// extended-resource analysis, so DRA causes are diagnosed separately.
func UsesDRA(pod *corev1.Pod) bool {
	return len(pod.Spec.ResourceClaims) > 0
}

// claimRef resolves a pod's resourceClaims[] entry to the actual ResourceClaim
// object name. Direct references use ResourceClaimName; template-generated claims
// are resolved via pod.status.resourceClaimStatuses.
func claimRef(pod *corev1.Pod, prc corev1.PodResourceClaim) (name string, fromTemplate bool) {
	if prc.ResourceClaimName != nil && *prc.ResourceClaimName != "" {
		return *prc.ResourceClaimName, false
	}
	for _, s := range pod.Status.ResourceClaimStatuses {
		if s.Name == prc.Name && s.ResourceClaimName != nil {
			return *s.ResourceClaimName, true
		}
	}
	return "", prc.ResourceClaimTemplateName != nil
}

func requestedClasses(claim *resourcev1.ResourceClaim) []string {
	set := map[string]bool{}
	for _, r := range claim.Spec.Devices.Requests {
		if r.Exactly != nil && r.Exactly.DeviceClassName != "" {
			set[r.Exactly.DeviceClassName] = true
		}
	}
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// AnalyzeDRA explains why a DRA pod's device claims aren't satisfied. It resolves
// each pod claim to its ResourceClaim and reports: not-yet-created, missing,
// unallocated (with concrete evidence from DeviceClasses/ResourceSlices), or
// allocated (no cause). claims may span namespaces; slices/classes are cluster-
// scoped. CEL device-selector matching is intentionally not performed here.
func AnalyzeDRA(pod *corev1.Pod, claims []resourcev1.ResourceClaim, slices []resourcev1.ResourceSlice, classes []resourcev1.DeviceClass) []Cause {
	byKey := map[string]*resourcev1.ResourceClaim{}
	for i := range claims {
		byKey[claims[i].Namespace+"/"+claims[i].Name] = &claims[i]
	}

	var causes []Cause
	for _, prc := range pod.Spec.ResourceClaims {
		name, _ := claimRef(pod, prc)
		if name == "" {
			causes = append(causes, Cause{
				Severity: Blocker,
				Title:    fmt.Sprintf("ResourceClaim for %q not created yet", prc.Name),
				Detail:   "The pod references a ResourceClaimTemplate, but the control plane hasn't materialized a ResourceClaim for it yet (or it can't be read).",
				Fix:      "Check the resource-claim controller and `kubectl get resourceclaims`; ensure the ResourceClaimTemplate exists in this namespace.",
			})
			continue
		}
		claim, ok := byKey[pod.Namespace+"/"+name]
		if !ok {
			causes = append(causes, Cause{
				Severity: Blocker,
				Title:    fmt.Sprintf("ResourceClaim %q not found", name),
				Detail:   fmt.Sprintf("Pod claim %q resolves to ResourceClaim %q, which doesn't exist in namespace %q.", prc.Name, name, pod.Namespace),
				Fix:      "Create the ResourceClaim, or fix the reference in the pod spec.",
			})
			continue
		}
		if claim.Status.Allocation != nil {
			continue // allocated — not the blocker
		}
		causes = append(causes, unallocatedClaimCause(name, claim, slices, classes))
	}
	return causes
}

// unallocatedClaimCause explains an unallocated claim with concrete evidence: a
// referenced DeviceClass that doesn't exist, no DRA driver publishing devices,
// or devices published but none allocatable.
func unallocatedClaimCause(name string, claim *resourcev1.ResourceClaim, slices []resourcev1.ResourceSlice, classes []resourcev1.DeviceClass) Cause {
	clsList := requestedClasses(claim)
	classesStr := "(none)"
	if len(clsList) > 0 {
		classesStr = strings.Join(clsList, ", ")
	}
	title := fmt.Sprintf("ResourceClaim %q is not allocated", name)
	detail := fmt.Sprintf("The scheduler couldn't allocate devices for this claim (requested DeviceClass(es): %s), so the pod stays Pending.", classesStr)
	fix := "Confirm the DRA driver is installed and publishing ResourceSlices (`kubectl get resourceslices`), that the DeviceClass exists, and that a matching device is free."

	if classes != nil {
		exists := map[string]bool{}
		for i := range classes {
			exists[classes[i].Name] = true
		}
		var missing []string
		for _, c := range clsList {
			if !exists[c] {
				missing = append(missing, c)
			}
		}
		if len(missing) > 0 {
			detail += fmt.Sprintf(" DeviceClass(es) not found in the cluster: %s — the request can never be satisfied.", strings.Join(missing, ", "))
			fix = fmt.Sprintf("Create the missing DeviceClass(es) (%s) or reference an existing one in the claim's request.", strings.Join(missing, ", "))
			return Cause{Severity: Blocker, Title: title, Detail: detail, Fix: fix}
		}
	}

	if slices != nil {
		total := 0
		for i := range slices {
			total += len(slices[i].Spec.Devices)
		}
		if total == 0 {
			detail += " No ResourceSlices publish any devices — the DRA driver for these classes isn't running or hasn't published its inventory."
			fix = "Install/repair the DRA driver (e.g. the NVIDIA DRA driver) and confirm `kubectl get resourceslices` lists devices."
		} else {
			detail += fmt.Sprintf(" %d device(s) are published cluster-wide, but none could be allocated — likely all already in use, or none match the request's selectors.", total)
			fix = "Free a matching device (scale down / delete claims holding them), add nodes with matching devices, or relax the request's selectors."
		}
	}

	return Cause{Severity: Blocker, Title: title, Detail: detail, Fix: fix}
}
