package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"
)

// Every flag form the reviews reported as "unknown flag" must parse.
func TestParseArgs_KubectlForms(t *testing.T) {
	cases := []struct {
		args  []string
		check func(t *testing.T, o options)
	}{
		{[]string{"--output=json"}, func(t *testing.T, o options) {
			if o.output != "json" {
				t.Errorf("output=%q", o.output)
			}
		}},
		{[]string{"-n=team-a", "web-0"}, func(t *testing.T, o options) {
			if o.namespace != "team-a" || o.podName != "web-0" {
				t.Errorf("ns=%q pod=%q", o.namespace, o.podName)
			}
		}},
		{[]string{"web-0", "--namespace=team-a", "-ojson"}, func(t *testing.T, o options) {
			if o.namespace != "team-a" || o.podName != "web-0" || o.output != "json" {
				t.Errorf("flags after the positional: %+v", o)
			}
		}},
		{[]string{"--as=system:serviceaccount:team-a:reader", "--as-group=devs", "--request-timeout=5s", "-A"}, func(t *testing.T, o options) {
			if *o.flags.Impersonate != "system:serviceaccount:team-a:reader" || len(*o.flags.ImpersonateGroup) != 1 || *o.flags.Timeout != "5s" || !o.allNamespaces {
				t.Errorf("impersonation/request-timeout not parsed: %+v", o)
			}
		}},
		{[]string{"--context", "lab", "--kubeconfig=/tmp/kc", "--server=https://10.0.0.1:6443", "--token=abc", "--insecure-skip-tls-verify"}, func(t *testing.T, o options) {
			if *o.flags.Context != "lab" || *o.flags.KubeConfig != "/tmp/kc" || *o.flags.APIServer != "https://10.0.0.1:6443" || *o.flags.BearerToken != "abc" || !*o.flags.Insecure {
				t.Errorf("connection flags not parsed")
			}
		}},
		{[]string{"--timeout=2m", "--no-color"}, func(t *testing.T, o options) {
			if o.timeout != 2*time.Minute || !o.noColor {
				t.Errorf("timeout=%s noColor=%v", o.timeout, o.noColor)
			}
		}},
		{nil, func(t *testing.T, o options) {
			if o.timeout != 60*time.Second || o.namespace != "" {
				t.Errorf("defaults: timeout=%s ns=%q", o.timeout, o.namespace)
			}
		}},
	}
	for _, c := range cases {
		o, err := parseArgs(c.args, &bytes.Buffer{})
		if err != nil {
			t.Errorf("%v: %v", c.args, err)
			continue
		}
		c.check(t, o)
	}
}

func TestParseArgs_Errors(t *testing.T) {
	for _, args := range [][]string{
		{"--bogus"},
		{"-o", "wide"},
		{"pod-a", "pod-b"},
		{"--timeout=0s"},
		{"-n"},
	} {
		if _, err := parseArgs(args, &bytes.Buffer{}); err == nil || errors.Is(err, errHelp) {
			t.Errorf("%v: want a usage error, got %v", args, err)
		}
	}
}

func TestParseArgs_HelpAndVersion(t *testing.T) {
	var out bytes.Buffer
	if _, err := parseArgs([]string{"--help"}, &out); !errors.Is(err, errHelp) {
		t.Fatalf("--help: %v", err)
	}
	for _, want := range []string{"--all-namespaces", "--timeout", "KUBECTL CONNECTION FLAGS", "--as string", "--request-timeout", "EXIT CODES"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("help missing %q", want)
		}
	}
	out.Reset()
	if _, err := parseArgs([]string{"--version"}, &out); !errors.Is(err, errVersion) || !strings.HasPrefix(out.String(), "kubectl-why-pending ") {
		t.Fatalf("--version: %v %q", err, out.String())
	}
}
