package main

import (
	"reflect"
	"testing"
)

func TestDisputeResolveArgsReadsTheThreeResolutions(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want map[string]any
	}{
		{"keep", []string{"abc", "keep"}, map[string]any{"id": "abc", "resolution": "keep"}},
		{"accept with challenger", []string{"abc", "accept", "--challenger", "def"},
			map[string]any{"id": "abc", "resolution": "accept_challenger", "challenger": "def"}},
		{"merge takes the rest as text", []string{"abc", "merge", "runs", "on", "6432", "--path", "memory/ops.md"},
			map[string]any{"id": "abc", "resolution": "merge", "text": "runs on 6432", "path": "memory/ops.md"}},
	}
	for _, c := range cases {
		got, usage, ok := disputeResolveArgs(c.args)
		if !ok {
			t.Fatalf("%s: refused with %q", c.name, usage)
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: body = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestDisputeResolveArgsRefusesBadInput(t *testing.T) {
	for _, args := range [][]string{
		{},
		{"abc"},
		{"abc", "maybe"},
		{"abc", "merge"},
		{"abc", "merge", "--path", "x.md"},
	} {
		if _, _, ok := disputeResolveArgs(args); ok {
			t.Errorf("args %v accepted, want refused", args)
		}
	}
}
