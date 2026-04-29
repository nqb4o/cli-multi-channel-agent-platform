// Command platform is the F13 CLI client for the Skill Registry.
//
// Usage:
//
//	platform [--registry-url URL] skills {search,info,list,install,update,check}
//
// Configuration via environment variables:
//
//	PLATFORM_REGISTRY_URL  — registry base URL (default: http://127.0.0.1:8090)
//	PLATFORM_TOKEN         — bearer token for publish (not needed for read-only)
//	PLATFORM_SKILLS_DIR    — install root (default: ~/.platform/skills)
//	PLATFORM_VERIFIER      — "inprocess" | "cosign" | "always-accept"
package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

func main() {
	if err := rootCmd().Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "platform: %v\n", err)
		os.Exit(1)
	}
}

func rootCmd() *cobra.Command {
	var registryURL string

	root := &cobra.Command{
		Use:   "platform",
		Short: "CLI-first multi-provider agent platform — F13 skills client",
		// PersistentPreRunE is called before every subcommand.
		// We stash the override into an env var so subcommand helpers can read it.
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			if registryURL != "" {
				os.Setenv("PLATFORM_REGISTRY_URL", registryURL)
			}
			return nil
		},
	}

	root.PersistentFlags().StringVar(&registryURL, "registry-url", "",
		"Override PLATFORM_REGISTRY_URL for this invocation")

	root.AddCommand(skillsCmd())

	return root
}
