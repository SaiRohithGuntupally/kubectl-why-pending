package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/sairohithg/kubectl-why-pending/pkg/diagnose"
)

type writer struct {
	color bool
}

func newWriter(noColor bool) *writer {
	return &writer{color: useColor(noColor)}
}

// useColor enables ANSI color only on a real terminal and when not disabled.
func useColor(noColor bool) bool {
	if noColor || os.Getenv("NO_COLOR") != "" {
		return false
	}
	fi, err := os.Stdout.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

func (w *writer) c(code, s string) string {
	if !w.color {
		return s
	}
	return "\033[" + code + "m" + s + "\033[0m"
}

func (w *writer) report(r diagnose.Result) {
	bold := func(s string) string { return w.c("1", s) }
	dim := func(s string) string { return w.c("2", s) }

	fmt.Println()
	fmt.Printf("%s %s/%s  ", bold("Pod"), r.Namespace, bold(r.PodName))
	fmt.Printf("%s\n", dim(fmt.Sprintf("(requests %s CPU / %s memory)",
		diagnose.FormatCPU(r.Request.CPUMilli), diagnose.FormatMem(r.Request.MemBytes))))
	fmt.Println(dim(strings.Repeat("─", 64)))

	if len(r.Causes) == 0 {
		fmt.Println("  No blocking cause found by static analysis.")
	}
	for _, c := range r.Causes {
		icon := w.severityColor(c.Severity, c.Severity.Icon())
		fmt.Printf("  %s %s\n", icon, bold(c.Title))
		for _, line := range strings.Split(c.Detail, "\n") {
			fmt.Printf("      %s\n", line)
		}
		fmt.Printf("      %s %s\n", w.c("32", "fix:"), c.Fix)
		fmt.Println()
	}

	if r.SchedulerEvent != "" {
		fmt.Printf("  %s %s\n", dim("scheduler:"), dim(r.SchedulerEvent))
	}
}

func (w *writer) severityColor(s diagnose.Severity, text string) string {
	switch s {
	case diagnose.Blocker:
		return w.c("31", text) // red
	case diagnose.Warning:
		return w.c("33", text) // yellow
	default:
		return w.c("36", text) // cyan
	}
}
