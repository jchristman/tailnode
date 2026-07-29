package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveStateDirHonoursFlag(t *testing.T) {
	got, err := resolveStateDir("/var/lib/tailnode")
	if err != nil {
		t.Fatal(err)
	}
	if got != "/var/lib/tailnode" {
		t.Fatalf("state dir = %q, want the flag value", got)
	}
}

// TestResolveStateDirIsIndependentOfArgv0 is the point of resolveStateDir.
// tsnet's own default is derived from the binary's filename, so a renamed copy
// registers as a new node and its subnet routes need approving again.
func TestResolveStateDirIsIndependentOfArgv0(t *testing.T) {
	var first string
	for _, argv0 := range []string{"/usr/bin/tailnode", "./tailnode.new", "tailnode-linux-amd64"} {
		saved := os.Args[0]
		os.Args[0] = argv0
		got, err := resolveStateDir("")
		os.Args[0] = saved
		if err != nil {
			t.Fatal(err)
		}
		if base := filepath.Base(got); base != defaultStateDirName {
			t.Errorf("argv[0]=%q gave state dir %q, want it to end in %q", argv0, got, defaultStateDirName)
		}
		if first == "" {
			first = got
			continue
		}
		if got != first {
			t.Errorf("argv[0]=%q gave state dir %q, but %q was returned earlier; "+
				"the node's identity must not depend on the binary's name", argv0, got, first)
		}
	}
}
