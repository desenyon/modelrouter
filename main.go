// modelrouter — intelligent model routing gateway.
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
		Short: "Intelligent model routing gateway — Luna first, Sol only when needed",
		Long: `modelrouter is an OpenAI-compatible gateway that routes every request
to the cheapest capable tier.

  Luna  — fast, efficient, default for most work
  Terra — balanced mid-tier
  Sol   — frontier intelligence, used sparingly

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
		Short: "Preview which tier a prompt would hit (no upstream call)",
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
			engine := router.New(cfg)
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
			fmt.Printf("tier:     %s\n", dec.Tier)
			fmt.Printf("model:    %s\n", dec.UpstreamModel)
			fmt.Printf("mode:     %s\n", dec.Mode)
			fmt.Printf("score:    %.3f\n", dec.Score)
			fmt.Printf("reasons:  %s\n", strings.Join(dec.Reasons, "; "))
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
			engine := router.New(cfg)
			type row struct {
				ID          string `json:"id"`
				Upstream    string `json:"upstream,omitempty"`
				Description string `json:"description"`
			}
			rows := []row{}
			for _, e := range engine.Registry().Catalog() {
				up := ""
				switch e.ID {
				case "luna":
					up = cfg.Models.Luna
				case "terra":
					up = cfg.Models.Terra
				case "sol":
					up = cfg.Models.Sol
				}
				rows = append(rows, row{ID: e.ID, Upstream: up, Description: e.Description})
			}
			if jsonOut {
				enc := json.NewEncoder(os.Stdout)
				enc.SetIndent("", "  ")
				return enc.Encode(rows)
			}
			fmt.Println("virtual models")
			for _, r := range rows {
				if r.Upstream != "" {
					fmt.Printf("  %-8s → %s\n    %s\n", r.ID, r.Upstream, r.Description)
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
			fmt.Println("modelrouter 1.0.0 — routing gateway")
		},
	}

	// Default to serve when no subcommand — gateway-first UX.
	root.RunE = serveCmd.RunE
	root.Flags().AddFlagSet(serveCmd.Flags())

	root.AddCommand(serveCmd, routeCmd, modelsCmd, versionCmd)
	if err := root.Execute(); err != nil {
		os.Exit(1)
	}
}
