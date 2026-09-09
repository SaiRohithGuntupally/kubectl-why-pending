package diagnose

import (
	"fmt"
	"sort"
	"strings"

	resourcev1 "k8s.io/api/resource/v1"
)

// Device taints (k8s 1.37+, KEP-5055) are the DRA analogue of node taints: a
// driver can taint a device inline in its ResourceSlice, or an admin can taint
// devices cluster-wide with a DeviceTaintRule. A claim whose request doesn't
// tolerate a NoSchedule/NoExecute taint can never be allocated on that device,
// which is invisible in the scheduler's "no devices available" wording.
//
// This file computes the effective taints per published device (inline +
// matching rules) and reports which devices are blocked for a claim's requests.

// taintSource is one taint on a device together with where it came from —
// an empty Rule means the driver put it in the ResourceSlice itself.
type taintSource struct {
	Taint resourcev1.DeviceTaint
	Rule  string // DeviceTaintRule name, or "" for an inline slice taint
}

// String renders a taint the way `kubectl describe node` does (key=value:Effect).
func (s taintSource) String() string {
	t := s.Taint
	if t.Value != "" {
		return fmt.Sprintf("%s=%s:%s", t.Key, t.Value, t.Effect)
	}
	return fmt.Sprintf("%s:%s", t.Key, t.Effect)
}

// publishedDevice is one device from a ResourceSlice with its resolved taints.
type publishedDevice struct {
	Driver, Pool, Name string
	Taints             []taintSource
}

// blocksScheduling reports whether a taint effect prevents new allocations.
// Like node taints, NoExecute implies NoSchedule. None and any unknown future
// effect are treated as no-op, as the API contract requires.
func blocksScheduling(e resourcev1.DeviceTaintEffect) bool {
	return e == resourcev1.DeviceTaintEffectNoSchedule || e == resourcev1.DeviceTaintEffectNoExecute
}

// ruleMatches implements DeviceTaintSelector: every set field must match; a
// nil selector matches nothing; an empty selector matches every device.
func ruleMatches(sel *resourcev1.DeviceTaintSelector, d publishedDevice) bool {
	if sel == nil {
		return false
	}
	if sel.Driver != nil && *sel.Driver != d.Driver {
		return false
	}
	if sel.Pool != nil && *sel.Pool != d.Pool {
		return false
	}
	if sel.Device != nil && *sel.Device != d.Name {
		return false
	}
	return true
}

// publishedDevices flattens the slices into devices with their effective taints:
// the driver's inline taints plus every DeviceTaintRule whose selector matches.
func publishedDevices(slices []resourcev1.ResourceSlice, rules []resourcev1.DeviceTaintRule) []publishedDevice {
	var out []publishedDevice
	for i := range slices {
		s := &slices[i]
		for j := range s.Spec.Devices {
			dev := s.Spec.Devices[j]
			pd := publishedDevice{Driver: s.Spec.Driver, Pool: s.Spec.Pool.Name, Name: dev.Name}
			for _, t := range dev.Taints {
				pd.Taints = append(pd.Taints, taintSource{Taint: t})
			}
			for r := range rules {
				rule := &rules[r]
				if !ruleMatches(rule.Spec.DeviceSelector, pd) {
					continue
				}
				pd.Taints = append(pd.Taints, taintSource{Rule: rule.Name, Taint: rule.Spec.Taint})
			}
			out = append(out, pd)
		}
	}
	return out
}

// deviceTolerationTolerates mirrors tolerationTolerates for DRA: Equal is the
// default operator, an empty key with Exists matches every key, and an empty
// effect matches all effects.
func deviceTolerationTolerates(t resourcev1.DeviceToleration, taint resourcev1.DeviceTaint) bool {
	if t.Effect != "" && t.Effect != taint.Effect {
		return false
	}
	if t.Key == "" {
		return t.Operator == resourcev1.DeviceTolerationOpExists
	}
	if t.Key != taint.Key {
		return false
	}
	op := t.Operator
	if op == "" {
		op = resourcev1.DeviceTolerationOpEqual
	}
	switch op {
	case resourcev1.DeviceTolerationOpExists:
		return true
	case resourcev1.DeviceTolerationOpEqual:
		return t.Value == taint.Value
	}
	return false
}

// untoleratedDeviceTaints returns the taints on a device (with a scheduling
// effect) that none of the request's tolerations cover.
func untoleratedDeviceTaints(tolerations []resourcev1.DeviceToleration, d publishedDevice) []taintSource {
	var bad []taintSource
	for _, ts := range d.Taints {
		if !blocksScheduling(ts.Taint.Effect) {
			continue
		}
		tolerated := false
		for _, tol := range tolerations {
			if deviceTolerationTolerates(tol, ts.Taint) {
				tolerated = true
				break
			}
		}
		if !tolerated {
			bad = append(bad, ts)
		}
	}
	return bad
}

// requestTolerations lists each request in the claim with the tolerations that
// apply to it. A FirstAvailable request is satisfied by any one of its
// subrequests, so the union of their tolerations is the permissive reading.
type requestTolerations struct {
	Name        string
	Tolerations []resourcev1.DeviceToleration
}

func claimRequestTolerations(claim *resourcev1.ResourceClaim) []requestTolerations {
	var out []requestTolerations
	for _, r := range claim.Spec.Devices.Requests {
		rt := requestTolerations{Name: r.Name}
		if r.Exactly != nil {
			rt.Tolerations = append(rt.Tolerations, r.Exactly.Tolerations...)
		}
		for _, sub := range r.FirstAvailable {
			rt.Tolerations = append(rt.Tolerations, sub.Tolerations...)
		}
		out = append(out, rt)
	}
	return out
}

// deviceTaintVerdict summarises the taint picture for one claim against every
// published device. Blocked lists the untolerated taints, aggregated across
// devices, that keep at least one request off some device; AllBlocked is true
// when every published device is unusable by at least one request — the case
// where the taints alone explain the Pending state.
type deviceTaintVerdict struct {
	Total      int
	Blocked    int // devices unusable by some request because of an untolerated taint
	AllBlocked bool
	Reasons    []string // rendered "taint (via ...) on N device(s)", sorted
}

func analyzeDeviceTaints(claim *resourcev1.ResourceClaim, devices []publishedDevice) deviceTaintVerdict {
	v := deviceTaintVerdict{Total: len(devices)}
	if len(devices) == 0 {
		return v
	}
	reqs := claimRequestTolerations(claim)
	if len(reqs) == 0 {
		// Nothing to match against; treat the claim as tolerating nothing.
		reqs = []requestTolerations{{Name: ""}}
	}

	type key struct{ taint, rule string }
	count := map[key]int{}
	for _, d := range devices {
		blocked := false
		seen := map[key]bool{}
		for _, r := range reqs {
			for _, bad := range untoleratedDeviceTaints(r.Tolerations, d) {
				blocked = true
				k := key{bad.String(), bad.Rule}
				if !seen[k] {
					seen[k] = true
					count[k]++
				}
			}
		}
		if blocked {
			v.Blocked++
		}
	}
	v.AllBlocked = v.Blocked == v.Total

	for k, n := range count {
		src := "set by the driver in its ResourceSlice"
		if k.rule != "" {
			src = fmt.Sprintf("via DeviceTaintRule %q", k.rule)
		}
		v.Reasons = append(v.Reasons, fmt.Sprintf("%s (%s) on %d device(s)", k.taint, src, n))
	}
	sort.Strings(v.Reasons)
	return v
}

// deviceTaintDetail renders the verdict as sentences for the cause detail and
// returns the fix to go with it. ok=false means taints don't explain anything.
func deviceTaintDetail(v deviceTaintVerdict) (detail, fix string, ok bool) {
	if v.Blocked == 0 {
		return "", "", false
	}
	reasons := strings.Join(v.Reasons, "; ")
	if v.AllBlocked {
		detail = fmt.Sprintf(" All %d published device(s) carry device taints this claim doesn't tolerate: %s. A tainted device is skipped for new allocations exactly like a tainted node, so the claim can't be satisfied until the taint is tolerated or removed.", v.Total, reasons)
		fix = "Add a matching toleration to the claim's device request (spec.devices.requests[].exactly.tolerations), or remove the taint — delete/edit the DeviceTaintRule, or fix the device so the driver clears its taint. Check `kubectl get devicetaintrules` and `kubectl get resourceslices -o yaml`."
		return detail, fix, true
	}
	detail = fmt.Sprintf(" %d of %d published device(s) carry device taints this claim doesn't tolerate (%s), so only %d device(s) were actually candidates — likely all in use, or none match the request's selectors.", v.Blocked, v.Total, reasons, v.Total-v.Blocked)
	fix = "Free one of the untainted matching devices, add a toleration to the claim's device request if the tainted devices are acceptable, or remove the taint / DeviceTaintRule once the hardware is healthy."
	return detail, fix, true
}
