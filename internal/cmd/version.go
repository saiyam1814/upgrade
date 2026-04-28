package cmd

import (
	"fmt"
	"runtime"
	"runtime/debug"

	"github.com/spf13/cobra"
)

// Set by ldflags at release time.
var (
	version  = "dev"
	commit   = ""
	dataDate = "" // pluto/kubent versions.yaml snapshot date
)

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print upgrade version, commit, and rules-data snapshot date",
		Run: func(cmd *cobra.Command, args []string) {
			rev := commit
			if rev == "" {
				if info, ok := debug.ReadBuildInfo(); ok {
					for _, s := range info.Settings {
						if s.Key == "vcs.revision" {
							rev = s.Value
						}
					}
				}
			}
			fmt.Printf("upgrade %s\n", version)
			if rev != "" {
				fmt.Printf("commit:    %s\n", rev)
			}
			if dataDate != "" {
				fmt.Printf("rules:     %s (run 'upgrade refresh-rules' to update)\n", dataDate)
			}
			fmt.Printf("go:        %s %s/%s\n", runtime.Version(), runtime.GOOS, runtime.GOARCH)
		},
	}
}
