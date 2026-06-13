package main

import (
	"fmt"
	"os"
)

// parseArgs is a small hand-rolled parser so flags can appear before or after
// the positional pod name (kubectl plugins receive args in any order).
func parseArgs(args []string) (options, error) {
	var o options
	needValue := func(i int, name string) (string, int, error) {
		if i+1 >= len(args) {
			return "", i, fmt.Errorf("flag %s needs a value", name)
		}
		return args[i+1], i + 1, nil
	}

	for i := 0; i < len(args); i++ {
		a := args[i]
		var err error
		switch {
		case a == "-h" || a == "--help":
			fmt.Print(usage)
			os.Exit(0)
		case a == "-A" || a == "--all-namespaces":
			o.allNamespaces = true
		case a == "--no-color":
			o.noColor = true
		case a == "-n" || a == "--namespace":
			o.namespace, i, err = needValue(i, a)
		case a == "--context":
			o.context, i, err = needValue(i, a)
		case a == "--kubeconfig":
			o.kubeconfig, i, err = needValue(i, a)
		case len(a) > 1 && a[0] == '-':
			return o, fmt.Errorf("unknown flag %q", a)
		default:
			if o.podName != "" {
				return o, fmt.Errorf("unexpected extra argument %q", a)
			}
			o.podName = a
		}
		if err != nil {
			return o, err
		}
	}
	return o, nil
}
