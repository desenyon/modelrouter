package tui

import (
	"fmt"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/desenyon/modelrouter/internal/api"
)

func testModels() []api.Model {
	return []api.Model{
		{
			ID: "acme/foo-large", Name: "Acme: Foo Large", Created: 1781007515,
			ContextLength: 1_000_000,
			Architecture: api.Architecture{
				Modality:         "text+image->text",
				InputModalities:  []string{"text", "image"},
				OutputModalities: []string{"text"},
			},
			Pricing:             api.Pricing{Prompt: "0.00001", Completion: "0.00005"},
			SupportedParameters: []string{"tools", "reasoning"},
			Description:         "A big model.",
		},
		{
			ID: "acme/bar-mini:free", Name: "Acme: Bar Mini (free)", Created: 1700000000,
			ContextLength: 32768,
			Architecture: api.Architecture{
				Modality:         "text->text",
				InputModalities:  []string{"text"},
				OutputModalities: []string{"text"},
			},
			Pricing: api.Pricing{Prompt: "0", Completion: "0"},
		},
	}
}

func pump(t *testing.T, a *App, msgs ...tea.Msg) {
	t.Helper()
	for _, m := range msgs {
		model, _ := a.Update(m)
		if app, ok := model.(*App); ok {
			*a = *app
		}
	}
}

func TestFullFlow(t *testing.T) {
	a := NewApp(api.New())
	pump(t, a,
		tea.WindowSizeMsg{Width: 120, Height: 40},
		dataMsg{models: testModels(), providers: []api.Provider{{Name: "Acme Cloud", Slug: "acme", Headquarters: "US"}}},
		splashDoneMsg{},
	)

	out := a.View()
	if !strings.Contains(out, "acme/foo-large") {
		t.Fatalf("models table missing row:\n%s", out)
	}
	if !strings.Contains(out, "modelrouter") {
		t.Fatal("header missing")
	}

	// filter to free models only
	pump(t, a, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'f'}})
	out = a.View()
	if strings.Contains(out, "acme/foo-large") || !strings.Contains(out, "bar-mini") {
		t.Fatalf("free filter failed:\n%s", out)
	}
	pump(t, a, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'f'}})

	// fuzzy search
	pump(t, a,
		tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'/'}},
		tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("foolrg")},
		tea.KeyMsg{Type: tea.KeyEnter},
	)
	if got := a.modelsView.Selected(); got == nil || got.ID != "acme/foo-large" {
		t.Fatalf("fuzzy search selected %v", got)
	}

	// open detail and feed endpoints
	pump(t, a, tea.KeyMsg{Type: tea.KeyEnter})
	if a.detail == nil {
		t.Fatal("detail view did not open")
	}
	up := 99.9
	pump(t, a, endpointsMsg{id: "acme/foo-large", data: &api.ModelEndpoints{
		ID: "acme/foo-large",
		Endpoints: []api.Endpoint{{
			ProviderName: "Acme Cloud", ContextLength: 1_000_000,
			Pricing:      api.Pricing{Prompt: "0.00001", Completion: "0.00005"},
			UptimeLast1d: &up,
		}},
	}})
	out = a.detail.content()
	for _, want := range []string{"Acme Cloud", "$10.00", "99.90%", "PROVIDER ENDPOINTS"} {
		if !strings.Contains(out, want) {
			t.Fatalf("detail missing %q:\n%s", want, out)
		}
	}

	// back out, visit other tabs
	pump(t, a, tea.KeyMsg{Type: tea.KeyEsc})
	if a.detail != nil {
		t.Fatal("esc did not close detail")
	}
	pump(t, a, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'4'}})
	if !strings.Contains(a.View(), "Acme Cloud") {
		t.Fatal("providers tab missing provider")
	}
	pump(t, a, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'5'}})
	if !strings.Contains(a.View(), "TOP AUTHORS") {
		t.Fatal("stats tab missing chart")
	}

	// benchmarks tab + detail embedding
	slug := "acme/foo-large"
	pump(t, a, benchMsg{data: &api.Benchmarks{
		AA: map[string][]api.AAScore{
			"intelligence": {{UID: "acme/foo-large-x", Permaslug: "acme/foo-large-x", HeuristicSlug: &slug, Name: "Foo Large", Score: 64.9}},
			"coding":       {{UID: "acme/foo-large-x", Permaslug: "acme/foo-large-x", HeuristicSlug: &slug, Name: "Foo Large", Score: 57.2}},
			"math":         {{UID: "acme/bar-mini", Permaslug: "acme/bar-mini", Name: "Bar Mini", Score: 81.0}},
		},
		DA: map[string][]api.DARow{
			"models-website": {{OpenrouterID: "acme/foo-large", DisplayName: "Foo Large", Score: 1362, WinRate: 65.8}},
		},
		CostPerRequest: map[string]float64{"acme/foo-large": 1.9512},
	}})
	pump(t, a, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'3'}})
	out = a.View()
	for _, want := range []string{"AA INTELLIGENCE", "AA MATH", "64.9", "DESIGN ARENA", "COST PER REQUEST"} {
		if !strings.Contains(out, want) {
			t.Fatalf("benchmarks tab missing %q:\n%s", want, out)
		}
	}
	pump(t, a,
		tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'1'}},
		tea.KeyMsg{Type: tea.KeyEnter},
	)
	if a.detail == nil {
		t.Fatal("detail did not reopen")
	}
	dout := a.detail.content()
	for _, want := range []string{"BENCHMARKS", "intelligence", "64.9", "website", "1362", "$1.9512"} {
		if !strings.Contains(dout, want) {
			t.Fatalf("detail missing benchmark %q:\n%s", want, dout)
		}
	}
	pump(t, a, tea.KeyMsg{Type: tea.KeyEsc})

	// rankings tab with injected leaderboard data
	ch := 0.42
	pump(t, a, rankingsMsg{data: &api.Rankings{
		ModelDays: []api.ModelDayStat{
			{Date: "2026-06-10 00:00:00", ModelPermaslug: "acme/foo-large", Variant: "standard",
				PromptTokens: 9e11, CompletionTokens: 1e11, Requests: 5e6, Change: &ch},
			{Date: "2026-06-10 00:00:00", ModelPermaslug: "acme/bar-mini", Variant: "free",
				PromptTokens: 4e10, CompletionTokens: 1e10, Requests: 2e6},
		},
		Apps: api.AppRankings{Week: []api.AppRank{
			{Rank: 1, TotalRequests: 12345678, TotalTokens: "851103783681",
				App: api.AppInfo{Title: "Hermes Agent", MainURL: "https://example.com"}},
		}},
		MarketShare: []api.SharePoint{{X: "2026-06-08", Ys: map[string]int64{"acme": 9e11, "others": 1e11}}},
		Performance: []api.PerfRow{{ID: "acme/foo-large", RequestCount: 934740,
			P50Latency: 3777, P50Throughput: 89.5, BestLatencyProvider: "Google", BestThroughputProvider: "Azure"}},
	}})
	pump(t, a, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'2'}})
	out = a.View()
	for _, want := range []string{"TOP MODELS BY TOKENS", "acme/bar-mini:free", "MARKET SHARE", "Hermes Agent", "PERFORMANCE", "3777ms"} {
		if !strings.Contains(out, want) {
			t.Fatalf("rankings tab missing %q:\n%s", want, out)
		}
	}
	// cycle apps period day -> week -> month
	pump(t, a, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'p'}})
	if !strings.Contains(a.View(), "per month") {
		t.Fatal("apps period cycle failed")
	}

	// the 5-minute tick must trigger a refetch and reschedule itself
	if _, cmd := a.Update(rankingsTickMsg{}); cmd == nil {
		t.Fatal("rankings tick did not schedule a refresh")
	}

	// help overlay toggles on ? and closes on any key
	pump(t, a, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'?'}})
	if !strings.Contains(a.View(), "NAVIGATION") {
		t.Fatal("help overlay did not open")
	}
	pump(t, a, tea.KeyMsg{Type: tea.KeyEsc})
	if strings.Contains(a.View(), "NAVIGATION") {
		t.Fatal("help overlay did not close")
	}
}

func TestRefreshGenerationAndStatePreservation(t *testing.T) {
	a := NewApp(api.New())
	pump(t, a,
		tea.WindowSizeMsg{Width: 120, Height: 40},
		dataMsg{models: testModels()},
		splashDoneMsg{},
		tea.KeyMsg{Type: tea.KeyDown},
	)
	selected := a.modelsView.Selected()
	if selected == nil {
		t.Fatal("expected a selected model")
	}
	selectedID := selected.ID

	a.generation = 2
	updated := testModels()
	updated[0].Description = "fresh"
	pump(t, a, dataMsg{models: updated, generation: 2})
	if got := a.modelsView.Selected(); got == nil || got.ID != selectedID {
		t.Fatalf("refresh lost selection: %#v", got)
	}

	stale := testModels()
	stale[0].Description = "stale"
	pump(t, a, dataMsg{models: stale, generation: 1})
	if a.models[0].Description != "fresh" {
		t.Fatal("older async response overwrote newer catalog data")
	}
}

func TestFailedLiveRefreshKeepsPriorLeaderboardSnapshot(t *testing.T) {
	a := NewApp(api.New())
	pump(t, a,
		tea.WindowSizeMsg{Width: 120, Height: 40},
		dataMsg{models: testModels()},
		splashDoneMsg{},
		rankingsMsg{data: &api.Rankings{
			ModelDays: []api.ModelDayStat{{Date: "2026-07-29", ModelPermaslug: "acme/foo", PromptTokens: 100}},
		}},
	)
	a.generation = 1
	pump(t, a,
		rankingsMsg{err: fmt.Errorf("upstream down"), generation: 1},
		tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'2'}},
	)
	out := a.View()
	if !strings.Contains(out, "showing prior rankings snapshot") || !strings.Contains(out, "acme/foo") {
		t.Fatalf("prior snapshot was not retained:\n%s", out)
	}
}

func TestDetailScrolls(t *testing.T) {
	a := NewApp(api.New())
	pump(t, a,
		tea.WindowSizeMsg{Width: 120, Height: 30},
		dataMsg{models: testModels(), providers: nil},
		splashDoneMsg{},
		tea.KeyMsg{Type: tea.KeyEnter}, // open detail of first row
	)
	if a.detail == nil {
		t.Fatal("detail did not open")
	}
	// long endpoint list forces overflow
	eps := make([]api.Endpoint, 50)
	for i := range eps {
		eps[i] = api.Endpoint{ProviderName: fmt.Sprintf("Provider %d", i),
			Pricing: api.Pricing{Prompt: "0.00001", Completion: "0.00002"}}
	}
	pump(t, a, endpointsMsg{id: a.detail.model.ID, data: &api.ModelEndpoints{ID: a.detail.model.ID, Endpoints: eps}})

	pump(t, a, tea.KeyMsg{Type: tea.KeyDown})
	if a.detail.vp.YOffset == 0 {
		t.Fatal("keyboard down did not scroll detail view")
	}
	before := a.detail.vp.YOffset
	pump(t, a, tea.MouseMsg{Action: tea.MouseActionPress, Button: tea.MouseButtonWheelDown})
	if a.detail.vp.YOffset <= before {
		t.Fatal("mouse wheel did not scroll detail view")
	}
	pump(t, a, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'G'}})
	if a.detail.ScrollPercent() < 0.99 {
		t.Fatal("G did not jump to bottom")
	}
	pump(t, a, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'g'}})
	if a.detail.vp.YOffset != 0 {
		t.Fatal("g did not jump to top")
	}
}

func TestTableMouseWheel(t *testing.T) {
	a := NewApp(api.New())
	pump(t, a,
		tea.WindowSizeMsg{Width: 120, Height: 40},
		dataMsg{models: testModels(), providers: nil},
		splashDoneMsg{},
	)
	pump(t, a, tea.MouseMsg{Action: tea.MouseActionPress, Button: tea.MouseButtonWheelDown})
	if cur, _ := a.modelsView.CursorInfo(); cur < 2 {
		t.Fatalf("wheel did not move table cursor, at %d", cur)
	}
}

func TestNarrowTerminal(t *testing.T) {
	a := NewApp(api.New())
	pump(t, a,
		tea.WindowSizeMsg{Width: 70, Height: 20},
		dataMsg{models: testModels(), providers: []api.Provider{{Name: "Acme Cloud", Slug: "acme"}}},
		splashDoneMsg{},
	)
	if out := a.View(); !strings.Contains(out, "acme") {
		t.Fatalf("narrow render broke:\n%s", out)
	}
	pump(t, a, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'4'}})
	if out := a.View(); !strings.Contains(out, "Acme Cloud") {
		t.Fatalf("narrow provider render broke:\n%s", out)
	}
}
