// modelrouter — complex-yet-efficient model routing gateway.
//
// Point any OpenAI-compatible client at the gateway with model "auto".
// Requests are classified and sent to Luna by default; Sol is used only
// when the task actually needs frontier intelligence.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/desenyon/modelrouter/internal/classifier"
	"github.com/desenyon/modelrouter/internal/config"
	"github.com/desenyon/modelrouter/internal/gateway"
	"github.com/desenyon/modelrouter/internal/health"
	"github.com/desenyon/modelrouter/internal/router"
)

func main() {
	var (
		configPath string
		modeFlag   string
		jsonOut    bool
	)

	root := &cobra.Command{
		Use:   "modelrouter",
		Short: "Complex-yet-efficient model routing gateway — Luna first, Sol only when needed",
		Long: `modelrouter is an OpenAI-compatible gateway that routes every request
to the cheapest capable tier.

  Luna  — fast, efficient, default for most work
  Terra — balanced mid-tier
  Sol   — frontier intelligence, used sparingly

Pipeline: cache → features → score → policy → tier → candidate/circuit →
proxy → cascade → adapt.

Point your app at the gateway with model "auto" (or "router") and optional
optimize_for: cost | balance | intelligence.`,
		SilenceUsage: true,
	}
	root.PersistentFlags().StringVarP(&configPath, "config", "c", "", "path to config YAML")
	root.PersistentFlags().BoolVar(&jsonOut, "json", false, "emit JSON")

	serveCmd := &cobra.Command{
		Use:   "serve",
		Short: "Start the OpenAI-compatible routing gateway",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load(configPath)
			if err != nil {
				return err
			}
			if modeFlag != "" {
				cfg.Router.DefaultMode = config.Mode(strings.ToLower(modeFlag))
				if err := cfg.Validate(); err != nil {
					return err
				}
			}
			srv := gateway.New(cfg, nil)
			return srv.ListenAndServe()
		},
	}
	serveCmd.Flags().StringVar(&modeFlag, "mode", "", "override default mode: cost|balance|intelligence")

	routeCmd := &cobra.Command{
		Use:   "route [prompt...]",
		Short: "Preview full routing decision trace (no upstream call)",
		Args:  cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load(configPath)
			if err != nil {
				return err
			}
			if modeFlag != "" {
				cfg.Router.DefaultMode = config.Mode(strings.ToLower(modeFlag))
			}
			prompt := strings.Join(args, " ")
			if prompt == "" {
				b, err := os.ReadFile("/dev/stdin")
				if err == nil && len(b) > 0 {
					prompt = string(b)
				}
			}
			if strings.TrimSpace(prompt) == "" {
				return fmt.Errorf("provide a prompt as args or on stdin")
			}
			engine := router.New(cfg, health.New())
			model, _ := cmd.Flags().GetString("model")
			dec := engine.Route(router.RouteInput{
				Model: model,
				Mode:  cfg.ModeOrDefault(),
				Req: classifier.Request{
					Messages: []classifier.Message{{Role: "user", Content: prompt}},
				},
			})
			if jsonOut {
				enc := json.NewEncoder(os.Stdout)
				enc.SetIndent("", "  ")
				return enc.Encode(dec)
			}
			fmt.Printf("tier:        %s\n", dec.Tier)
			fmt.Printf("model:       %s\n", dec.UpstreamModel)
			fmt.Printf("mode:        %s\n", dec.Mode)
			fmt.Printf("score:       %.3f\n", dec.Score)
			fmt.Printf("thresholds:  luna≤%.2f terra≤%.2f\n", dec.LunaMax, dec.TerraMax)
			fmt.Printf("circuit:     %s\n", dec.CircuitState)
			fmt.Printf("cascade:     %v\n", dec.AllowCascade)
			fmt.Printf("candidates:  %s\n", strings.Join(dec.Candidates, ", "))
			if len(dec.Policy.Hits) > 0 {
				fmt.Printf("policy:      %s\n", strings.Join(dec.Policy.Hits, ", "))
			}
			fmt.Printf("reasons:     %s\n", strings.Join(dec.Reasons, "; "))
			fmt.Printf("features:    chars=%d tokens≈%d tools=%d hard=%d easy=%d agent=%v structured=%v\n",
				dec.Features.Chars, dec.Features.EstTokens, dec.Features.Tools,
				dec.Features.HardMarkers, dec.Features.EasyMarkers, dec.Features.AgentLike, dec.Features.StructuredOut)
			return nil
		},
	}
	routeCmd.Flags().String("model", "auto", "virtual model: auto|luna|terra|sol")
	routeCmd.Flags().StringVar(&modeFlag, "mode", "", "cost|balance|intelligence")

	modelsCmd := &cobra.Command{
		Use:   "models",
		Short: "List virtual router models and upstream tier mappings",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load(configPath)
			if err != nil {
				return err
			}
			engine := router.New(cfg, health.New())
			type row struct {
				ID          string   `json:"id"`
				Upstream    string   `json:"upstream,omitempty"`
				Fallbacks   []string `json:"fallbacks,omitempty"`
				Description string   `json:"description"`
			}
			rows := []row{}
			for _, e := range engine.Registry().Catalog() {
				up := ""
				var fb []string
				switch e.ID {
				case "luna":
					up = cfg.Models.Luna.Primary
					fb = cfg.Models.Luna.Fallbacks
				case "terra":
					up = cfg.Models.Terra.Primary
					fb = cfg.Models.Terra.Fallbacks
				case "sol":
					up = cfg.Models.Sol.Primary
					fb = cfg.Models.Sol.Fallbacks
				}
				rows = append(rows, row{ID: e.ID, Upstream: up, Fallbacks: fb, Description: e.Description})
			}
			if jsonOut {
				enc := json.NewEncoder(os.Stdout)
				enc.SetIndent("", "  ")
				return enc.Encode(rows)
			}
			fmt.Println("virtual models")
			for _, r := range rows {
				if r.Upstream != "" {
					fmt.Printf("  %-8s → %s\n", r.ID, r.Upstream)
					if len(r.Fallbacks) > 0 {
						fmt.Printf("           fallbacks: %s\n", strings.Join(r.Fallbacks, ", "))
					}
					fmt.Printf("    %s\n", r.Description)
				} else {
					fmt.Printf("  %-8s\n    %s\n", r.ID, r.Description)
				}
			}
			fmt.Printf("\ndefault mode: %s\n", cfg.ModeOrDefault())
			return nil
		},
	}

	versionCmd := &cobra.Command{
		Use:   "version",
		Short: "Print version",
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Println("modelrouter 2.0.0 — complex-yet-efficient routing gateway")
		},
	}

	root.RunE = serveCmd.RunE
	root.Flags().AddFlagSet(serveCmd.Flags())

	root.AddCommand(serveCmd, routeCmd, modelsCmd, versionCmd)
	if err := root.Execute(); err != nil {
		os.Exit(1)
	}
}
