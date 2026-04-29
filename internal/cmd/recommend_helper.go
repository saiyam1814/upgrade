package cmd

import (
	"fmt"
	"os"

	"github.com/saiyam1814/upgrade/internal/finding"
	"github.com/saiyam1814/upgrade/internal/recommend"
	"github.com/saiyam1814/upgrade/internal/report"
	"github.com/saiyam1814/upgrade/internal/ui"
)

// emitRecommendation prints the smart "→ Next: ..." footer in human
// format only. Structured outputs (json/md/sarif) stay clean for
// machine consumers.
func emitRecommendation(format report.Format, c recommend.Context) {
	if format != report.FormatHuman {
		return
	}
	hint := recommend.NextStep(c)
	if hint == "" {
		return
	}
	fmt.Fprintln(os.Stdout)
	fmt.Fprintln(os.Stdout, ui.Bold("→ Next: ")+hint)
}

// hasCategory reports whether any finding belongs to the given category.
func hasCategory(fs []finding.Finding, cat finding.Category) bool {
	for _, f := range fs {
		if f.Category == cat {
			return true
		}
	}
	return false
}
