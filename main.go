// modelrouter — a terminal explorer for the OpenRouter model catalog.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"

	"github.com/spf13/cobra"

	"github.com/desenyon/modelrouter/internal/api"
	"github.com/desenyon/modelrouter/internal/cli"
	"github.com/desenyon/modelrouter/internal/tui"
)

func main() {
	client := api.New()

	var (
		jsonOut  bool
		sortFlag string
		desc     bool
		search   string
		freeOnly bool
		tools    bool
		modality string
		limit    int
		refresh  bool
	)

	root := &cobra.Command{
		Use:   "modelrouter",
		Short: "Beautiful terminal explorer for the OpenRouter catalog",
		Long:  "modelrouter — browse every model, provider endpoint, price, and capability on OpenRouter.\nRun with no arguments for the interactive TUI; use subcommands for scriptable output.",
		RunE: func(cmd *cobra.Command, args []string) error {
			return tui.Run(client)
		},
		SilenceUsage: true,
	}
	root.PersistentFlags().BoolVar(&refresh, "refresh", false, "bypass the local cache")

	loadModels := func() ([]api.Model, error) { return client.Models(refresh) }

	modelsCmd := &cobra.Command{
		Use:   "models",
		Short: "List models (filterable, sortable, JSON-able)",
		RunE: func(cmd *cobra.Command, args []string) error {
			models, err := loadModels()
			if err != nil {
				return err
			}
			models = api.FilterModels(models, api.Filter{
				Query: search, FreeOnly: freeOnly, Tools: tools, Modality: modality,
			})
			if search == "" {
				api.SortModels(models, api.ParseSortKey(sortFlag), desc)
			}
			if limit > 0 && len(models) > limit {
				models = models[:limit]
			}
			if jsonOut {
				return cli.JSON(models)
			}
			cli.PrintModels(models)
			return nil
		},
	}
	modelsCmd.Flags().BoolVar(&jsonOut, "json", false, "emit JSON")
	modelsCmd.Flags().StringVar(&sortFlag, "sort", "newest", "sort: newest|name|prompt|completion|context")
	modelsCmd.Flags().BoolVar(&desc, "desc", false, "reverse sort order")
	modelsCmd.Flags().StringVar(&search, "search", "", "fuzzy search query")
	modelsCmd.Flags().BoolVar(&freeOnly, "free", false, "free models only")
	modelsCmd.Flags().BoolVar(&tools, "tools", false, "models with tool calling only")
	modelsCmd.Flags().StringVar(&modality, "input", "", "require input modality: text|image|audio|file")
	modelsCmd.Flags().IntVar(&limit, "limit", 0, "max rows (0 = all)")

	modelCmd := &cobra.Command{
		Use:   "model <id>",
		Short: "Full detail for one model, including per-provider endpoints",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			models, err := loadModels()
			if err != nil {
				return err
			}
			m := api.FindModel(models, args[0])
			if m == nil {
				return fmt.Errorf("no model matching %q", args[0])
			}
			eps, epsErr := client.Endpoints(m.ID)
			var mb *api.ModelBench
			if bench, err := client.Benchmarks(refresh); err == nil {
				mb = bench.ForModel(*m)
			}
			if jsonOut {
				return cli.JSON(map[string]any{"model": m, "endpoints": eps, "benchmarks": mb})
			}
			cli.PrintModelDetail(*m, eps, epsErr)
			cli.PrintModelBench(mb)
			return nil
		},
	}
	modelCmd.Flags().BoolVar(&jsonOut, "json", false, "emit JSON")

	providersCmd := &cobra.Command{
		Use:   "providers",
		Short: "List inference providers",
		RunE: func(cmd *cobra.Command, args []string) error {
			providers, err := client.Providers(refresh)
			if err != nil {
				return err
			}
			if jsonOut {
				return cli.JSON(providers)
			}
			cli.PrintProviders(providers)
			return nil
		},
	}
	providersCmd.Flags().BoolVar(&jsonOut, "json", false, "emit JSON")

	statsCmd := &cobra.Command{
		Use:   "stats",
		Short: "Catalog-wide stats: authors, pricing, context, capabilities",
		RunE: func(cmd *cobra.Command, args []string) error {
			models, err := loadModels()
			if err != nil {
				return err
			}
			providers, err := client.Providers(refresh)
			if err != nil {
				return err
			}
			cli.PrintStats(api.ComputeStats(models, providers))
			return nil
		},
	}

	var appsPeriod string
	rankingsCmd := &cobra.Command{
		Use:   "rankings [models|apps|share|perf|all]",
		Short: "Live leaderboards: token usage, top apps, market share, performance",
		Long:  "Live leaderboard data from OpenRouter's (unofficial) frontend API:\ntoken usage per model, top apps, author market share, and latency/throughput leaders.",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			r, err := client.Rankings(refresh)
			if err != nil {
				return err
			}
			what := "models"
			if len(args) > 0 {
				what = args[0]
			}
			if jsonOut {
				switch what {
				case "models":
					return cli.JSON(r.TopModels())
				case "apps":
					return cli.JSON(r.Apps)
				case "share":
					return cli.JSON(r.MarketShare)
				case "perf":
					return cli.JSON(r.Performance)
				default:
					return cli.JSON(r)
				}
			}
			switch what {
			case "models":
				cli.PrintTopModels(r, limit)
			case "apps":
				cli.PrintApps(r, appsPeriod)
			case "share":
				cli.PrintMarketShare(r)
			case "perf":
				cli.PrintPerformance(r, limit)
			case "all":
				cli.PrintTopModels(r, 20)
				cli.PrintMarketShare(r)
				cli.PrintApps(r, appsPeriod)
				cli.PrintPerformance(r, 15)
			default:
				return fmt.Errorf("unknown rankings view %q (models|apps|share|perf|all)", what)
			}
			return nil
		},
	}
	rankingsCmd.Flags().BoolVar(&jsonOut, "json", false, "emit JSON")
	rankingsCmd.Flags().IntVar(&limit, "limit", 25, "max rows (0 = all)")
	rankingsCmd.Flags().StringVar(&appsPeriod, "period", "week", "apps period: day|week|month")

	benchCmd := &cobra.Command{
		Use:     "benchmarks",
		Aliases: []string{"bench"},
		Short:   "Artificial Analysis scores + Design Arena results",
		RunE: func(cmd *cobra.Command, args []string) error {
			b, err := client.Benchmarks(refresh)
			if err != nil {
				return err
			}
			if jsonOut {
				return cli.JSON(b)
			}
			cli.PrintBenchmarks(b)
			return nil
		},
	}
	benchCmd.Flags().BoolVar(&jsonOut, "json", false, "emit JSON")

	var withEndpoints bool
	var outFile string
	exportCmd := &cobra.Command{
		Use:   "export",
		Short: "Dump the entire catalog as JSON (optionally every model's endpoints)",
		RunE: func(cmd *cobra.Command, args []string) error {
			models, err := loadModels()
			if err != nil {
				return err
			}
			providers, err := client.Providers(refresh)
			if err != nil {
				return err
			}
			dump := map[string]any{"models": models, "providers": providers}
			if withEndpoints {
				eps := make(map[string]*api.ModelEndpoints, len(models))
				var mu sync.Mutex
				var wg sync.WaitGroup
				sem := make(chan struct{}, 8)
				for _, m := range models {
					wg.Add(1)
					go func(id string) {
						defer wg.Done()
						sem <- struct{}{}
						defer func() { <-sem }()
						if e, err := client.Endpoints(id); err == nil {
							mu.Lock()
							eps[id] = e
							mu.Unlock()
						}
					}(m.ID)
				}
				wg.Wait()
				dump["endpoints"] = eps
				fmt.Fprintf(os.Stderr, "fetched endpoints for %d/%d models\n", len(eps), len(models))
			}
			if outFile != "" {
				b, err := json.MarshalIndent(dump, "", "  ")
				if err != nil {
					return err
				}
				if err := os.WriteFile(outFile, b, 0o644); err != nil {
					return err
				}
				fmt.Fprintln(os.Stderr, "wrote "+outFile)
				return nil
			}
			return cli.JSON(dump)
		},
	}
	exportCmd.Flags().BoolVar(&withEndpoints, "endpoints", false, "also fetch per-provider endpoints for every model")
	exportCmd.Flags().StringVar(&outFile, "out", "", "write to file instead of stdout")

	cacheCmd := &cobra.Command{
		Use:   "clear-cache",
		Short: "Delete the local response cache",
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := client.ClearCache(); err != nil {
				return err
			}
			fmt.Println("cache cleared:", client.CacheDir)
			return nil
		},
	}

	root.AddCommand(modelsCmd, modelCmd, providersCmd, statsCmd, rankingsCmd, benchCmd, exportCmd, cacheCmd)
	if err := root.Execute(); err != nil {
		os.Exit(1)
	}
}
