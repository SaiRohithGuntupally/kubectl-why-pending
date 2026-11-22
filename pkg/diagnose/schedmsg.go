package diagnose

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// SchedulerReason is one clause of a FailedScheduling message, e.g.
// "3 Insufficient cpu" -> {Count: 3, Text: "Insufficient cpu", Category: "cpu"}.
// Count is 0 for pod-level (PreFilter) reasons that apply to every node, such
// as "pod has unbound immediate PersistentVolumeClaims".
type SchedulerReason struct {
	Count    int    `json:"count,omitempty"`
	Text     string `json:"text"`
	Category string `json:"category"`
	// Modelled is false for filters this tool doesn't re-check, where the
	// scheduler's word is the only evidence.
	Modelled bool `json:"modelled"`
}

// ParsedSchedulerMessage is the structured form of a FailedScheduling message.
type ParsedSchedulerMessage struct {
	Total   int               `json:"total"` // N in "0/N nodes are available"
	Reasons []SchedulerReason `json:"reasons"`
}

var schedMsgRe = regexp.MustCompile(`^0/(\d+) nodes are available:\s*(.*)$`)
var leadingCountRe = regexp.MustCompile(`^(\d+) (.+)$`)

// ParseSchedulerMessage parses the kube-scheduler FailedScheduling message.
// The format has been stable since 1.2x:
//
//	0/6 nodes are available: 1 node(s) didn't match Pod's node affinity/selector,
//	3 node(s) had untolerated taint {node-role.kubernetes.io/etcd: true},
//	2 Insufficient cpu. preemption: 0/6 nodes are available: ...
//
// Reasons are sorted by the scheduler, comma separated; taint clauses can
// contain commas inside {...}, so a clause only starts at "<digits> ".
// Returns ok=false for messages in other shapes (custom schedulers, binding
// errors, "skip schedule deleting pod", ...).
func ParseSchedulerMessage(msg string) (ParsedSchedulerMessage, bool) {
	msg = strings.TrimSpace(msg)
	if i := strings.Index(msg, " preemption: "); i >= 0 {
		msg = msg[:i]
	}
	m := schedMsgRe.FindStringSubmatch(msg)
	if m == nil {
		return ParsedSchedulerMessage{}, false
	}
	total, _ := strconv.Atoi(m[1])
	body := strings.TrimSuffix(strings.TrimSpace(m[2]), ".")
	out := ParsedSchedulerMessage{Total: total}
	if body == "" {
		return out, true
	}

	var clauses []string
	for _, part := range strings.Split(body, ", ") {
		if len(clauses) > 0 && !leadingCountRe.MatchString(part) && !isPodLevelReason(part) {
			clauses[len(clauses)-1] += ", " + part // comma inside a taint {...}
			continue
		}
		clauses = append(clauses, part)
	}
	for _, c := range clauses {
		c = strings.TrimSuffix(strings.TrimSpace(c), ".")
		r := SchedulerReason{Text: c}
		if mm := leadingCountRe.FindStringSubmatch(c); mm != nil {
			r.Count, _ = strconv.Atoi(mm[1])
			r.Text = mm[2]
		}
		r.Category, r.Modelled = categorize(r.Text)
		out.Reasons = append(out.Reasons, r)
	}
	return out, true
}

func isPodLevelReason(s string) bool {
	s = strings.ToLower(s)
	return strings.HasPrefix(s, "pod has ") || strings.HasPrefix(s, "persistentvolumeclaim ") ||
		strings.HasPrefix(s, "resourceclaim") || strings.HasPrefix(s, "unbound ")
}

// categorize maps a scheduler reason to a category and whether this tool
// models it. Patterns are substrings so they survive minor rewording across
// releases (1.33 through 1.37 tested in schedmsg_test.go).
func categorize(text string) (string, bool) {
	t := strings.ToLower(text)
	switch {
	case strings.Contains(t, "were unschedulable"):
		return "cordoned", true
	case strings.Contains(t, "taint") && (strings.Contains(t, "not-ready") || strings.Contains(t, "unreachable")):
		return "not-ready", true
	case strings.Contains(t, "device taint"):
		return "dra", true
	case strings.Contains(t, "taint"):
		return "taint", true
	case strings.Contains(t, "node affinity/selector"):
		return "placement", true
	case strings.Contains(t, "requested node name"):
		return "node-name", false
	case strings.Contains(t, "free ports"):
		return "host-port", false
	case strings.HasPrefix(t, "insufficient cpu"):
		return "cpu", true
	case strings.HasPrefix(t, "insufficient memory"):
		return "memory", true
	case strings.HasPrefix(t, "insufficient ephemeral-storage"):
		return "ephemeral-storage", true
	case strings.HasPrefix(t, "insufficient "):
		return "extended-resource", true
	case strings.Contains(t, "too many pods"):
		return "max-pods", true
	case strings.Contains(t, "topology spread"):
		return "topology-spread", true
	case strings.Contains(t, "anti-affinity") || strings.Contains(t, "pod affinity"):
		return "pod-affinity", true
	case strings.Contains(t, "volume node affinity conflict"), strings.Contains(t, "volume zone"),
		strings.Contains(t, "available persistent volumes"), strings.Contains(t, "enough free storage"):
		return "volume-topology", false
	case strings.Contains(t, "max volume count"):
		return "volume-attach-limit", false
	case strings.Contains(t, "readwriteoncepod"), strings.Contains(t, "no available disk"):
		return "volume-conflict", false
	case strings.Contains(t, "unbound immediate persistentvolumeclaims"), strings.Contains(t, "persistentvolumeclaim"):
		return "pvc", true
	case strings.Contains(t, "claim") || strings.Contains(t, "device"):
		return "dra", true
	}
	return "other", false
}

// unmodelledHint explains, per category, what the scheduler checks that this
// tool doesn't, and what to look at.
var unmodelledHint = map[string][2]string{
	"host-port":           {"another pod on those nodes already holds a requested hostPort", "Find the holder with `kubectl get pods -A -o wide` on the node (DaemonSets such as ingress controllers often bind 80/443), drop the hostPort, or use a Service."},
	"volume-topology":     {"the pod's PersistentVolumes are pinned (PV nodeAffinity / zone) to nodes it can't use, or no matching PV or free storage exists", "Check `kubectl get pv <name> -o yaml` (spec.nodeAffinity): local-path / OpenEBS / TopoLVM volumes live on one node; if that node is gone, the PV must be recreated."},
	"volume-attach-limit": {"those nodes hit the CSI driver's max attached volumes", "Check `kubectl get csinode <node> -o yaml` (allocatable.count) against the volumes already attached."},
	"volume-conflict":     {"a ReadWriteOncePod or exclusive volume is already in use", "Find the pod already using the claim and stop it first."},
	"node-name":           {"the pod targets a specific node name", "Check spec.nodeName / the hostname affinity."},
	"other":               {"a filter this tool doesn't recognise (a newer Kubernetes reason or a scheduler plugin)", "Trust the scheduler's message; `kubectl describe pod` shows it in full."},
}

// crossCheck compares the scheduler's last verdict with this tool's analysis.
//
//   - No blocker here, but the scheduler rejected nodes: the scheduler is the
//     ground truth, so each reason becomes a Blocker, labelled as either a
//     filter this tool doesn't model or a disagreement with its model.
//   - Blockers here, and the scheduler also names reasons this tool doesn't
//     model: add one Warning so the extra cause isn't lost.
//
// It doesn't flag "we found X, the scheduler didn't say X": the scheduler stops
// at the first failing filter per node and runs them in a different order, so
// that comparison produces false alarms.
func crossCheck(res *Result, msg string) []Cause {
	parsed, ok := ParseSchedulerMessage(msg)
	if !ok || len(parsed.Reasons) == 0 {
		return nil
	}
	res.SchedulerReasons = parsed.Reasons

	if res.HasBlocker() {
		var extra []string
		for _, r := range parsed.Reasons {
			if !r.Modelled {
				extra = append(extra, describeReason(r, parsed.Total))
			}
		}
		if len(extra) == 0 {
			return nil
		}
		return []Cause{{
			Severity: Warning,
			Title:    "The scheduler also reports a reason this tool doesn't check",
			Detail:   "From the scheduler's last attempt: " + strings.Join(extra, "; ") + ". Fixing the causes above may not be enough.",
			Fix:      "Read the scheduler line below and address these too.",
		}}
	}

	var causes []Cause
	for _, r := range parsed.Reasons {
		what := describeReason(r, parsed.Total)
		if !r.Modelled {
			h, ok := unmodelledHint[r.Category]
			if !ok {
				h = unmodelledHint["other"]
			}
			causes = append(causes, Cause{
				Severity: Blocker,
				Title:    "Scheduler: " + what,
				Detail:   fmt.Sprintf("The scheduler's last attempt rejected the pod because %s. This tool doesn't model that check, so its own analysis found nothing; the scheduler's message is the ground truth.", h[0]),
				Fix:      h[1],
			})
			continue
		}
		causes = append(causes, Cause{
			Severity: Blocker,
			Title:    "Scheduler: " + what + " (this tool's model disagrees)",
			Detail:   "The scheduler's last attempt rejected the pod for this reason, but this tool's re-check of the same filter found nodes that pass. The usual causes: the cluster changed since that attempt, usage this tool can't see (terminating pods, in-flight resizes, a scheduler plugin config), or a gap in this tool. Trust the scheduler.",
			Fix:      "Re-run in a minute; if the disagreement stays, please open a misdiagnosis issue with `-o json` output.",
		})
	}
	return causes
}

func describeReason(r SchedulerReason, total int) string {
	if r.Count == 0 {
		return r.Text
	}
	if strings.HasPrefix(r.Text, "node(s) ") || strings.HasPrefix(r.Text, "node ") {
		return fmt.Sprintf("%d/%d %s", r.Count, total, r.Text)
	}
	return fmt.Sprintf("%d/%d node(s): %s", r.Count, total, r.Text)
}
