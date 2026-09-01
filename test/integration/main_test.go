//go:build integration

package integration

import (
	"fmt"
	"os"
	"testing"
)

// TestMain does the work that is the same for every scenario -- see Setup --
// once for the package, because a package is what TestMain wraps.
//
// No *testing.T here, so a setup failure is an exit rather than a test
// failure. That is the right shape: nothing can run without it.
func TestMain(m *testing.M) {
	root, err := Setup()
	if err != nil {
		fmt.Fprintf(os.Stderr, "integration: %v\n", err)
		os.Exit(1)
	}

	code := m.Run()

	Teardown(root)
	os.Exit(code)
}
