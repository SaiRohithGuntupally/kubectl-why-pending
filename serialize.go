package main

import (
	"encoding/json"
	"fmt"

	"sigs.k8s.io/yaml"

	"github.com/sairohithg/kubectl-why-pending/pkg/diagnose"
)

// structured reports whether the output format is a machine-readable one.
func structured(format string) bool {
	return format == "json" || format == "yaml"
}

// emit writes the results as JSON or YAML. Always an array (even for one pod) so
// consumers can parse the output shape unconditionally.
func emit(format string, results []diagnose.Result) error {
	switch format {
	case "json":
		b, err := json.MarshalIndent(results, "", "  ")
		if err != nil {
			return err
		}
		fmt.Println(string(b))
	case "yaml":
		// sigs.k8s.io/yaml marshals via JSON tags, so the struct tags and the
		// Severity string encoding are honored.
		b, err := yaml.Marshal(results)
		if err != nil {
			return err
		}
		fmt.Print(string(b))
	default:
		return fmt.Errorf("unknown output format %q", format)
	}
	return nil
}
