package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

var version = "dev"

var rootCmd = &cobra.Command{
	Use:   "miroxy",
	Short: "miroxy — LLM API gateway with multi-provider routing",
	Long: `miroxy — LLM API gateway with multi-provider routing

A single-binary proxy that sits between your AI clients and upstream
LLM providers (Gemini, DeepSeek, Anthropic, Bedrock, and more).

  • Translates Anthropic and OpenAI client protocols to any upstream format
  • Key pool rotation, 429 backoff, and circuit-breaking across providers
  • Fallback / round-robin / least-requests routing across providers
  • Hot reload config without dropping in-flight requests

Examples:
    miroxy serve                          Start with default config path
    miroxy serve -c /etc/miroxy.yaml      Start with explicit config
    miroxy serve --port 9090              Override listen port
    miroxy health                         Check running instance health
    miroxy stat                           Show credpool and routing stats
    miroxy reload                         Hot-reload config file`,
	SilenceUsage:  true,
	SilenceErrors: true,
}

func init() {
	rootCmd.Version = version
	rootCmd.SetVersionTemplate("miroxy {{.Version}}\n")

	// -v / --version handled by Cobra via rootCmd.Version above.
	// Override the default flag name to match headroom style.
	rootCmd.Flags().BoolP("version", "v", false, "show version and exit")
}

func Execute() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n\nRun 'miroxy --help' for usage.\n", err)
		os.Exit(1)
	}
}
