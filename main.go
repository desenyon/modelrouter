// modelrouter — an OpenAI-compatible gateway that routes every request to the
// (model, reasoning-effort) pair with the best expected value across OpenAI,
// Anthropic and Google Gemini.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/desenyon/modelrouter/internal/canon"
	"github.com/desenyon/modelrouter/internal/catalog"
	"github.com/desenyon/modelrouter/internal/config"
	"github.com/desenyon/modelrouter/internal/doctor"
	"github.com/desenyon/modelrouter/internal/embed"
	"github.com/desenyon/modelrouter/internal/eval"
	"github.com/desenyon/modelrouter/internal/gateway"
	"github.com/desenyon/modelrouter/internal/optimize"
	"github.com/desenyon/modelrouter/internal/predict"
	"github.com/desenyon/modelrouter/internal/provider"
)

func main() {
	var cfgPath string
	var jsonOut bool
	root := &cobra.Command{
		Use:   "modelrouter",
		Short: "Expected-value model router for OpenAI, Anthropic and Gemini",
		Long: `modelrouter is an OpenAI-compatible gateway. Send model "auto" and every
request is routed to the (model, reasoning-effort) pair that maximizes

    value × P(success) − cost − latency

subject to hard constraints. P(success) comes from a local embedding model
(pure Go, ~30µs) that estimates difficulty and skill mix, matched against
per-model abilities that are refined online from feedback.

Virtual models: auto, auto:cost, auto:quality, auto:fast,
                luna, terra, sol, astra (tiers), openai/auto, anthropic/auto, gemini/auto.
Any provider model id (e.g. claude-sonnet-5-5, gpt-6.1-sol) is routed directly.`,
		SilenceUsage: true,
	}
	root.PersistentFlags().StringVarP(&cfgPath, "config", "c", "", "path to config YAML")
	root.PersistentFlags().BoolVar(&jsonOut, "json", false, "emit JSON")

	load := func() (config.Config, error) { return config.Load(cfgPath) }

	var modeFlag string
	serve := &cobra.Command{
		Use:   "serve",
		Short: "Start the gateway",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := load()
			if err != nil {
				return err
			}
			if modeFlag != "" {
				cfg.Router.DefaultMode = modeFlag
				if err := cfg.Validate(); err != nil {
					return err
				}
			}
			log := slog.New(slog.NewTextHandler(os.Stderr, nil))
			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			srv, err := gateway.New(ctx, cfg, gateway.Options{Logger: log})
			if err != nil {
				return err
			}
			provs := srv.Providers()
			if len(provs) == 0 {
				log.Warn("no provider credentials configured: set OPENAI_API_KEY, ANTHROPIC_API_KEY and/or GEMINI_API_KEY")
			}
			b, l := srv.Router.Predictor.Size()
			log.Info("modelrouter "+gateway.Version, "listen", cfg.Listen, "providers", strings.Join(provs, ","),
				"mode", cfg.Router.DefaultMode, "exemplars", b, "learned", l, "budget_usd_per_hour", cfg.Budget.USDPerHour)
			hs := &http.Server{Addr: cfg.Listen, Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 120 * time.Second}
			bgDone := make(chan struct{})
			bgCtx, bgCancel := context.WithCancel(context.Background())
			go func() { srv.Background(bgCtx); close(bgDone) }()
			errc := make(chan error, 1)
			go func() { errc <- hs.ListenAndServe() }()
			select {
			case err := <-errc:
				bgCancel()
				<-bgDone
				return err
			case <-ctx.Done():
			}
			log.Info("shutting down")
			sctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			_ = hs.Shutdown(sctx)
			bgCancel()
			<-bgDone
			return nil
		},
	}
	serve.Flags().StringVar(&modeFlag, "mode", "", "default mode: cost|balance|quality|fast")

	var routeModel, routeMode string
	var configuredOnly bool
	route := &cobra.Command{
		Use:   "route [prompt...]",
		Short: "Show the routing decision for a prompt (no upstream call)",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := load()
			if err != nil {
				return err
			}
			prompt := strings.Join(args, " ")
			if prompt == "" {
				b, _ := io.ReadAll(io.LimitReader(os.Stdin, 8<<20))
				prompt = string(b)
			}
			if strings.TrimSpace(prompt) == "" {
				return errors.New("provide a prompt as arguments or on stdin")
			}
			srv, err := previewServer(cmd.Context(), cfg, configuredOnly)
			if err != nil {
				return err
			}
			req := &canon.Request{Model: routeModel, Router: canon.RouterOptions{Mode: routeMode},
				Messages: []canon.Message{{Role: canon.RoleUser, Parts: []canon.Part{{Type: canon.PartText, Text: prompt}}}}}
			dec, err := srv.Router.Route(req)
			if err != nil {
				return err
			}
			if jsonOut {
				return printJSON(map[string]any{"plan": dec.Plan, "routing_us": dec.RoutingTime.Microseconds()})
			}
			printPlan(dec.Plan, dec.RoutingTime)
			return nil
		},
	}
	route.Flags().StringVar(&routeModel, "model", "auto", "requested model (auto, auto:quality, sol, claude-opus-5-5, …)")
	route.Flags().StringVar(&routeMode, "mode", "", "cost|balance|quality|fast")
	route.Flags().BoolVar(&configuredOnly, "configured", false, "only consider providers with credentials")

	models := &cobra.Command{
		Use:   "models",
		Short: "List the model catalog",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := load()
			if err != nil {
				return err
			}
			cat, err := cfg.BuildCatalog()
			if err != nil {
				return err
			}
			if jsonOut {
				return printJSON(cat.All())
			}
			ready := map[string]bool{"openai": cfg.Providers.OpenAI.APIKey != "", "anthropic": cfg.Providers.Anthropic.APIKey != "", "gemini": cfg.Providers.Gemini.APIKey != ""}
			tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
			fmt.Fprintln(tw, "TIER\tMODEL\tAUTO\tKEY\tCONTEXT\tOUT\t$IN/M\t$OUT/M\tEFFORTS")
			now := time.Now()
			for _, m := range cat.All() {
				p := m.PriceAt(now)
				auto, key := "", "-"
				if m.Enabled {
					auto = "yes"
				}
				if ready[m.Provider] {
					key = "ok"
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%.2f\t%.2f\t%s\n", m.Tier, m.ID, auto, key, kTok(m.Context), kTok(m.MaxOutput), p.Input, p.Output, strings.Join(m.Efforts, ","))
			}
			tw.Flush()
			fmt.Printf("\ncatalog verified against provider docs on %s; run `modelrouter doctor` to check against live APIs\n", catalog.Snapshot)
			return nil
		},
	}

	evalCmd := &cobra.Command{
		Use:   "eval",
		Short: "Measure predictor accuracy and simulate routing on held-out prompts",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := load()
			if err != nil {
				return err
			}
			em, err := embed.Ensure(cmd.Context(), cfg.Embedder.Dir, cfg.AutoDownload(), logf)
			if err != nil {
				return err
			}
			p, err := predict.New(em, predict.DefaultConfig())
			if err != nil {
				return err
			}
			cat, err := cfg.BuildCatalog()
			if err != nil {
				return err
			}
			held, err := eval.HeldOut()
			if err != nil {
				return err
			}
			loo := eval.LeaveOneOut(p, 0)
			gen := eval.Generalization(p, held, 5)
			objs := cfg.Objectives()
			var sims []eval.RoutingReport
			for _, m := range []optimize.Mode{optimize.ModeCost, optimize.ModeBalance, optimize.ModeQuality} {
				sims = append(sims, eval.SimulateRouting(cat, p, held, m, objs[m]))
			}
			if jsonOut {
				return printJSON(map[string]any{"leave_one_out": loo, "held_out": gen, "routing": sims})
			}
			eval.Print(os.Stdout, "predictor — leave-one-out on exemplar bank", loo)
			fmt.Println()
			eval.Print(os.Stdout, "predictor — held-out prompts (never seen)", gen)
			for _, s := range sims {
				fmt.Println()
				eval.PrintRouting(os.Stdout, s)
			}
			return nil
		},
	}

	var probe bool
	doc := &cobra.Command{
		Use:   "doctor",
		Short: "Verify credentials and that every catalog model exists upstream",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := load()
			if err != nil {
				return err
			}
			cat, err := cfg.BuildCatalog()
			if err != nil {
				return err
			}
			srv, err := gateway.New(cmd.Context(), cfg, gateway.Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
			if err != nil {
				return err
			}
			provs := map[string]provider.Provider{}
			for _, n := range srv.Providers() {
				provs[n] = srv.ProviderByName(n)
			}
			rep := doctor.Run(cmd.Context(), cfg, cat, provs, probe)
			if jsonOut {
				return printJSON(rep)
			}
			if err := embed.Present(embedDir(cfg)); err != nil {
				fmt.Printf("embedder: MISSING (%v)\n\n", err)
			} else {
				fmt.Printf("embedder: ok (%s)\n\n", embedDir(cfg))
			}
			doctor.Print(os.Stdout, rep)
			if rep.Problems > 0 {
				os.Exit(1)
			}
			return nil
		},
	}
	doc.Flags().BoolVar(&probe, "probe", false, "send one tiny live request to each auto-routed model (small cost)")

	emb := &cobra.Command{Use: "embedder", Short: "Manage the required local embedding model"}
	emb.AddCommand(&cobra.Command{
		Use:   "fetch",
		Short: "Download and verify the embedding model",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := load()
			if err != nil {
				return err
			}
			dir := embedDir(cfg)
			if err := embed.Fetch(cmd.Context(), dir, logf); err != nil {
				return err
			}
			fmt.Println("embedder ready at", dir)
			return nil
		},
	}, &cobra.Command{
		Use:   "status",
		Short: "Check the embedding model",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := load()
			if err != nil {
				return err
			}
			dir := embedDir(cfg)
			if err := embed.Present(dir); err != nil {
				return fmt.Errorf("embedder at %s: %w", dir, err)
			}
			fmt.Printf("ok: %s (%s@%s)\n", dir, embed.DefaultRepo, embed.DefaultRevision[:12])
			return nil
		},
	})

	version := &cobra.Command{
		Use:   "version",
		Short: "Print version",
		Run: func(*cobra.Command, []string) {
			fmt.Printf("modelrouter %s (catalog %s, embedder %s)\n", gateway.Version, catalog.Snapshot, embed.DefaultName)
		},
	}

	root.AddCommand(serve, route, models, evalCmd, doc, emb, version)
	root.RunE = serve.RunE
	root.Flags().AddFlagSet(serve.Flags())
	if err := root.Execute(); err != nil {
		os.Exit(1)
	}
}

// previewServer builds a gateway for offline decisions. By default all
// native providers are treated as available so the preview shows the
// unconstrained choice.
func previewServer(ctx context.Context, cfg config.Config, configuredOnly bool) (*gateway.Server, error) {
	opt := gateway.Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if !configuredOnly {
		opt.Providers = map[string]provider.Provider{"openai": nil, "anthropic": nil, "gemini": nil}
	}
	cfg.Telemetry.DecisionLog = ""
	return gateway.New(ctx, cfg, opt)
}

func printPlan(p *optimize.Plan, took time.Duration) {
	pr := p.Prediction
	c := p.Chosen
	fmt.Printf("→ %s  (tier %s", c.ModelID, c.Tier)
	if c.Effort != "" {
		fmt.Printf(", effort %s", c.Effort)
	}
	fmt.Printf(")\n\n")
	fmt.Printf("mode         %s   floor P≥%.2f   value $%.3f   λ %.2f\n", p.Mode, p.Objective.Floor, p.Objective.ValueUSD, p.Lambda)
	fmt.Printf("difficulty   %.3f ± %.3f  (semantic %.3f: knn %.3f, ridge %.3f; max sim %.2f) → risk-adjusted %.3f\n",
		pr.Difficulty, pr.Uncertainty, pr.Semantic, pr.KNN, pr.Ridge, pr.MaxSim, p.Difficulty)
	type kv struct {
		k string
		v float64
	}
	var axes []kv
	for k, v := range pr.Axes.Map() {
		if v >= 0.05 {
			axes = append(axes, kv{k, v})
		}
	}
	sort.Slice(axes, func(i, j int) bool { return axes[i].v > axes[j].v })
	var as []string
	for _, a := range axes {
		as = append(as, fmt.Sprintf("%s %.2f", a.k, a.v))
	}
	fmt.Printf("skills       %s\n", strings.Join(as, ", "))
	fmt.Printf("tokens       in ≈%d   out ≈%d\n", p.Features.InputTokens, pr.OutTokens)
	if len(pr.Adjust) > 0 {
		fmt.Printf("structure    %s\n", strings.Join(pr.Adjust, " "))
	}
	if len(pr.Neighbors) > 0 {
		fmt.Printf("neighbors    ")
		for i, n := range pr.Neighbors {
			if i > 0 {
				fmt.Printf("             ")
			}
			fmt.Printf("%.2f  d=%.2f  %s\n", n.Sim, n.D, n.Text)
		}
	}
	fmt.Println()
	tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "\tMODEL\tEFFORT\tP(success)\tEST COST\tEST LATENCY\tUTILITY")
	for _, x := range p.Table {
		mark := " "
		if x == c {
			mark = "→"
		} else if !x.MeetsFloor {
			mark = "·"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%.3f\t$%.5f\t%.1fs\t%+.5f\n", mark, x.ModelID, dash(x.Effort), x.P, x.CostUSD, x.LatencyMs/1000, x.Utility)
	}
	tw.Flush()
	var fb []string
	for _, f := range p.Fallbacks {
		fb = append(fb, f.ModelID)
	}
	fmt.Printf("\nfallbacks    %s\n", strings.Join(fb, " → "))
	if len(p.Excluded) > 0 {
		var ex []string
		for _, e := range p.Excluded {
			if e.Reason != "not_auto_routed" {
				ex = append(ex, e.ModelID+" ("+e.Reason+")")
			}
		}
		if len(ex) > 0 {
			fmt.Printf("excluded     %s\n", strings.Join(ex, ", "))
		}
	}
	fmt.Printf("decided in   %s  (· = below quality floor)\n", took.Round(time.Microsecond))
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func kTok(n int) string {
	if n >= 1_000_000 {
		return fmt.Sprintf("%.2gM", float64(n)/1e6)
	}
	return fmt.Sprintf("%dK", n/1000)
}

func embedDir(cfg config.Config) string {
	if cfg.Embedder.Dir != "" {
		return cfg.Embedder.Dir
	}
	return embed.DefaultDir()
}

func logf(f string, a ...any) { fmt.Fprintf(os.Stderr, f+"\n", a...) }

func printJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
