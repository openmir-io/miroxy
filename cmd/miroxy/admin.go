package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

var flagAdminAddr string   // --admin-addr flag shared by health/stat/reload
var flagAdminConfig string // -c flag for admin commands to read config

// resolveAdminAddr returns the admin base URL.
// Priority: --admin-addr flag > MIROXY_ADMIN_ADDR env > -c config file > default.
func resolveAdminAddr() string {
	if flagAdminAddr != "" {
		return ensureScheme(flagAdminAddr)
	}
	if v := os.Getenv("MIROXY_ADMIN_ADDR"); v != "" {
		return ensureScheme(v)
	}
	if flagAdminConfig != "" {
		if addr := adminAddrFromFile(flagAdminConfig); addr != "" {
			return ensureScheme(addr)
		}
	}
	return "http://127.0.0.1:9001"
}

// adminAddrFromFile reads only admin.addr from a config file without full
// validation or env-var expansion — avoids failures from unset API key vars.
func adminAddrFromFile(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var partial struct {
		Admin struct {
			Addr string `yaml:"addr"`
		} `yaml:"admin"`
	}
	if err := yaml.Unmarshal(data, &partial); err != nil {
		return ""
	}
	return partial.Admin.Addr
}

// ensureScheme prepends http:// if the address has no scheme.
func ensureScheme(addr string) string {
	if strings.HasPrefix(addr, "http://") || strings.HasPrefix(addr, "https://") {
		return addr
	}
	return "http://" + addr
}

// adminCall POSTs to the admin API and returns the response body.
// resolveAuthToken returns the token to use for admin API calls.
// Checks MIROXY_AUTH_TOKEN first (proxy client key), then MIROXY_ADMIN_TOKEN.
func resolveAuthToken() string {
	if t := os.Getenv("MIROXY_AUTH_TOKEN"); t != "" {
		return t
	}
	return os.Getenv("MIROXY_ADMIN_TOKEN")
}

func adminCall(path string) ([]byte, error) {
	return adminDo(http.MethodPost, path)
}

// adminGet makes an authenticated GET request to the admin API.
func adminGet(path string) ([]byte, error) {
	return adminDo(http.MethodGet, path)
}

func adminDo(method, path string) ([]byte, error) {
	base := resolveAdminAddr()
	url := base + path
	client := &http.Client{Timeout: 10 * time.Second}
	req, err := http.NewRequest(method, url, nil) //nolint:noctx
	if err != nil {
		return nil, err
	}
	if tok := resolveAuthToken(); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("cannot reach miroxy admin at %s\n  start one with: miroxy serve\n  or set MIROXY_ADMIN_ADDR / MIROXY_AUTH_TOKEN\n  error: %w", base, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("admin API returned HTTP %d: %s", resp.StatusCode, string(body))
	}
	return body, nil
}

// formatStat formats the /GetStatus JSON response as a human-readable report.
func formatStat(raw []byte) string {
	var s struct {
		Uptime   string `json:"uptime"`
		InFlight int64  `json:"in_flight"`
		Config   string `json:"config"`
		Models   []struct {
			ModelName     string `json:"model_name"`
			Provider      string `json:"provider"`
			ProviderModel string `json:"provider_model"`
			Strategy      string `json:"strategy"`
		} `json:"models"`
		CredPools []struct {
			Name string `json:"name"`
			Keys int    `json:"keys"`
		} `json:"credpools"`
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		return prettyJSON(raw) + "\n"
	}

	sep := strings.Repeat("=", 60)
	sub := strings.Repeat("-", 40)
	var b strings.Builder

	fmt.Fprintf(&b, "miroxy Status Report\n%s\n", sep)
	fmt.Fprintf(&b, "Uptime:     %s\n", s.Uptime)
	fmt.Fprintf(&b, "In-flight:  %d\n", s.InFlight)
	if s.Config != "" {
		fmt.Fprintf(&b, "Config:     %s\n", s.Config)
	}

	fmt.Fprintf(&b, "\nModel Routing\n%s\n", sub)
	if len(s.Models) == 0 {
		fmt.Fprintf(&b, "  (no models configured)\n")
	}
	for _, m := range s.Models {
		strategy := ""
		if m.Strategy != "" {
			strategy = "  [" + m.Strategy + "]"
		}
		fmt.Fprintf(&b, "  %-20s →  %s  /  %s%s\n",
			m.ModelName, m.Provider, m.ProviderModel, strategy)
	}

	fmt.Fprintf(&b, "\nKey Pools\n%s\n", sub)
	if len(s.CredPools) == 0 {
		fmt.Fprintf(&b, "  (no named credpools)\n")
	}
	for _, p := range s.CredPools {
		fmt.Fprintf(&b, "  %-20s  %d key(s)\n", p.Name+":", p.Keys)
	}

	fmt.Fprintf(&b, "\n")
	return b.String()
}

// prettyJSON re-indents a JSON response for human-readable output.
func prettyJSON(raw []byte) string {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return string(raw)
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return string(raw)
	}
	return string(b)
}

// ── miroxy health ─────────────────────────────────────────────────────────────

var healthCmd = &cobra.Command{
	Use:   "health",
	Short: "Check health of running miroxy instance",
	Long: `Check health of running miroxy instance.

Exits with code 0 when the instance is healthy, non-zero otherwise.

Address resolution order:
  1. --admin-addr flag
  2. MIROXY_ADMIN_ADDR env var
  3. admin.addr from -c config file
  4. default http://127.0.0.1:8090`,
	RunE: func(cmd *cobra.Command, _ []string) error {
		body, err := adminCall("/miroxy.v1.AdminService/GetHealth")
		if err != nil {
			return err
		}
		fmt.Println(prettyJSON(body))
		return nil
	},
}

// ── miroxy stat ───────────────────────────────────────────────────────────────

var statCmd = &cobra.Command{
	Use:   "stat",
	Short: "Show credpool and routing stats of running instance",
	Long: `Show credpool and routing stats of running miroxy instance.

Displays per-pool key health (healthy / rate-limited / circuit-open)
and the current model routing table.

Address resolution order:
  1. --admin-addr flag
  2. MIROXY_ADMIN_ADDR env var
  3. admin.addr from -c config file
  4. default http://127.0.0.1:8090`,
	RunE: func(cmd *cobra.Command, _ []string) error {
		body, err := adminCall("/miroxy.v1.AdminService/GetStatus")
		if err != nil {
			return err
		}
		fmt.Print(formatStat(body))
		return nil
	},
}

// ── miroxy reload ─────────────────────────────────────────────────────────────

var reloadCmd = &cobra.Command{
	Use:   "reload",
	Short: "Hot-reload config file in running instance",
	Long: `Signal a running miroxy instance to re-read its config file.

In-flight requests complete against the current config; new requests
use the updated config immediately after the swap.

Changes to server.port or admin.addr are rejected — those require
a full restart.

Address resolution order:
  1. --admin-addr flag
  2. MIROXY_ADMIN_ADDR env var
  3. admin.addr from -c config file
  4. default http://127.0.0.1:8090`,
	RunE: func(cmd *cobra.Command, _ []string) error {
		body, err := adminCall("/miroxy.v1.AdminService/Reload")
		if err != nil {
			return err
		}
		fmt.Println(prettyJSON(body))
		return nil
	},
}

// ── config command ────────────────────────────────────────────────────────────

func configGetCmd(use, short, path string) *cobra.Command {
	return &cobra.Command{
		Use:   use,
		Short: short,
		RunE: func(cmd *cobra.Command, _ []string) error {
			body, err := adminGet(path)
			if err != nil {
				return err
			}
			fmt.Println(prettyJSON(body))
			return nil
		},
	}
}

var configCmd = &cobra.Command{
	Use:   "config [providers|routes|credpools]",
	Short: "Show effective runtime configuration (keys masked)",
	Long: `Show miroxy's effective runtime configuration — all defaults filled in,
API keys masked to last 4 characters.

Requires MIROXY_AUTH_TOKEN env var (any value from auth.allowed_keys).

Sub-commands:
  providers   Show resolved provider definitions
  routes      Show model routes (including auto-discovered)
  credpools    Show credpools with masked keys

Without a sub-command, returns the full config.`,
	RunE: func(cmd *cobra.Command, _ []string) error {
		body, err := adminGet("/v1/config")
		if err != nil {
			return err
		}
		fmt.Println(prettyJSON(body))
		return nil
	},
}

func init() {
	managementCmds := []*cobra.Command{healthCmd, statCmd, reloadCmd}
	for _, cmd := range managementCmds {
		cmd.Flags().StringVar(&flagAdminAddr, "admin-addr", "", "admin server address (overrides config and env)")
		cmd.Flags().StringVarP(&flagAdminConfig, "config", "c", "", "config file to read admin.addr from")
		rootCmd.AddCommand(cmd)
	}

	configCmd.Flags().StringVar(&flagAdminAddr, "admin-addr", "", "admin server address (default: 127.0.0.1:9001)")
	configCmd.Flags().StringVarP(&flagAdminConfig, "config", "c", "", "config file to read admin.addr from")

	configCmd.AddCommand(
		configGetCmd("providers", "Show resolved provider definitions", "/v1/config/providers"),
		configGetCmd("routes", "Show model routes (including auto-discovered)", "/v1/config/routes"),
		configGetCmd("credpools", "Show credpools with masked keys", "/v1/config/credpools"),
	)
	rootCmd.AddCommand(configCmd)
}
