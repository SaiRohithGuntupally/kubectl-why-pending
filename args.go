package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/pflag"
	"k8s.io/cli-runtime/pkg/genericclioptions"
)

// errHelp / errVersion are returned by parseArgs when it printed help or the
// version, so main exits 0 without contacting the cluster.
var (
	errHelp    = errors.New("help requested")
	errVersion = errors.New("version requested")
)

// parseArgs parses the command line with pflag plus the standard kubectl
// connection flags (genericclioptions.ConfigFlags), so everything a kubectl
// user types works: --flag=value and -n=ns, --as/--as-group, --token,
// --server, --request-timeout, --insecure-skip-tls-verify, --cluster/--user.
// Flags may appear before or after the positional pod name.
func parseArgs(args []string, out io.Writer) (options, error) {
	var o options
	fs := pflag.NewFlagSet("kubectl-why-pending", pflag.ContinueOnError)
	fs.SetOutput(io.Discard) // errors are printed once by main, with usage
	fs.SortFlags = false

	o.flags = genericclioptions.NewConfigFlags(true)
	o.flags.AddFlags(fs)

	fs.BoolVarP(&o.allNamespaces, "all-namespaces", "A", false, "diagnose Pending pods in all namespaces")
	fs.StringVarP(&o.output, "output", "o", "", "output format: text (default), json, or yaml")
	fs.BoolVar(&o.noColor, "no-color", false, "disable colored output (also honors NO_COLOR)")
	fs.DurationVar(&o.timeout, "timeout", 60*time.Second, "deadline for the whole run (use --request-timeout for a single API call)")
	showVersion := fs.Bool("version", false, "print the version and exit")
	help := fs.BoolP("help", "h", false, "show this help")

	if err := fs.Parse(args); err != nil {
		return o, err
	}
	if *help {
		fmt.Fprint(out, usageText(fs))
		return o, errHelp
	}
	if *showVersion {
		fmt.Fprintln(out, "kubectl-why-pending", version)
		return o, errVersion
	}

	switch rest := fs.Args(); len(rest) {
	case 0:
	case 1:
		o.podName = rest[0]
	default:
		return o, fmt.Errorf("unexpected extra argument %q", rest[1])
	}
	switch o.output {
	case "", "text", "json", "yaml":
	default:
		return o, fmt.Errorf("invalid --output %q (want text, json, or yaml)", o.output)
	}
	if o.allNamespaces && o.podName != "" {
		return o, fmt.Errorf("-A can't be combined with a pod name; use -n <namespace> %s", o.podName)
	}
	if o.timeout <= 0 {
		return o, fmt.Errorf("--timeout must be positive, got %s", o.timeout)
	}
	if o.flags.Namespace != nil {
		o.namespace = *o.flags.Namespace
	}
	return o, nil
}

// usageText renders help with our flags first, then the standard kubectl
// connection flags.
func usageText(fs *pflag.FlagSet) string {
	ours := pflag.NewFlagSet("ours", pflag.ContinueOnError)
	conn := pflag.NewFlagSet("conn", pflag.ContinueOnError)
	ours.SortFlags, conn.SortFlags = false, true
	fs.VisitAll(func(f *pflag.Flag) {
		switch f.Name {
		case "all-namespaces", "output", "no-color", "timeout", "version", "help", "namespace", "context", "kubeconfig":
			ours.AddFlag(f)
		default:
			conn.AddFlag(f)
		}
	})
	var b strings.Builder
	b.WriteString(usageHeader)
	b.WriteString("FLAGS:\n")
	b.WriteString(ours.FlagUsages())
	b.WriteString("\nKUBECTL CONNECTION FLAGS:\n")
	b.WriteString(conn.FlagUsages())
	b.WriteString(usageFooter)
	return b.String()
}

const usageHeader = `kubectl why-pending: explain why pods are stuck Pending

USAGE:
  kubectl why-pending [pod] [flags]

  With no pod name, diagnoses every Pending pod in the namespace.

`

const usageFooter = `
EXIT CODES:
  0  no blocking cause found (or no Pending pods)
  1  at least one Pending pod has a blocking cause
  2  usage error
  3  runtime error (e.g. could not reach the cluster, or --timeout hit)
`

// printUsageError prints a parse error plus a pointer to --help.
func printUsageError(err error) {
	fmt.Fprintln(os.Stderr, "error:", err)
	fmt.Fprintln(os.Stderr, "Run 'kubectl why-pending --help' for usage.")
}
