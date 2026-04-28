// upgrade is a Kubernetes upgrade pre-flight CLI.
// It scans live clusters and manifest sources for time bombs that
// will detonate on the next minor-version upgrade — deprecated APIs,
// PDB drain deadlocks, addon/CNI/CSI incompatibilities, vCluster
// distro removals, and feature-gate / default-value behavior changes
// that no managed provider's upgrade insights catch today.
package main

import (
	"fmt"
	"os"

	"github.com/saiyam1814/upgrade/internal/cmd"
)

func main() {
	if err := cmd.NewRoot().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
