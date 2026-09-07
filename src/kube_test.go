package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestKubectlConfigDetection(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("KUBECONFIG", "")
	config := filepath.Join(home, ".kube", "config")
	if err := os.MkdirAll(filepath.Dir(config), 0700); err != nil {
		t.Fatal(err)
	}
	check := func(want string) {
		t.Helper()
		cmd := kubectl(context.Background(), "config", "current-context")
		var got string
		for _, entry := range cmd.Environ() {
			if value, ok := strings.CutPrefix(entry, "KUBECONFIG="); ok {
				got = value
			}
		}
		if got != want {
			t.Fatalf("KUBECONFIG = %q, want %q", got, want)
		}
	}
	check("") // No user config: preserve kubectl's own discovery.
	if err := os.WriteFile(config, nil, 0600); err != nil {
		t.Fatal(err)
	}
	check(config)
	explicit := filepath.Join(home, "first") + string(os.PathListSeparator) + filepath.Join(home, "second")
	t.Setenv("KUBECONFIG", explicit)
	check(explicit) // Preserve multi-file configs without adding --kubeconfig.
}
