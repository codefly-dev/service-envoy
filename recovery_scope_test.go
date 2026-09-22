package main

import (
	"fmt"
	"os"
	"testing"

	"github.com/codefly-dev/core/runners/dockerrun"
)

// The source-validation suite owns its fixture containers independently of
// the CLI grandparent that launched the Go test process.
func TestMain(m *testing.M) {
	code := func() int {
		dir, err := os.MkdirTemp("", "service-envoy-test-scope-*")
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		defer os.RemoveAll(dir)
		scope, err := dockerrun.NewContainerRecoveryScope(dir, dir, "service-envoy-tests")
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		if err := dockerrun.SetContainerRecoveryScope(scope); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		return m.Run()
	}()
	os.Exit(code)
}
