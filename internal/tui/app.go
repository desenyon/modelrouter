// Package tui implements the interactive Bubble Tea interface.
package tui

import (
	"fmt"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/progress"
	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/desenyon/modelrouter/internal/api"
)

const (
	tabModels = iota
	tabRankings
	tabBench
	tabProviders
	tabStats
	tabCount
)

var tabNames = []string{"Models", "Rankings", "Benchmarks", "Providers", "Stats"}

type dataMsg struct {
	models    []api.Model
	providers []api.Provider
	err       error
}

type endpointsMsg struct {
	id   string
	data *api.ModelEndpoints
	err  error
}

type rankingsMsg struct {
	data *api.Rankings
	err  error
}

type benchMsg struct {
	data *api.Benchmarks
	err  error
}

type rankingsTickMsg struct{}
type gradTickMsg struct{}
type splashDoneMsg struct{}
type flashClearMsg struct{}

func rankingsTick() tea.Cmd {
	return tea.Tick(api.RankingsTTL, func(time.Time) tea.Msg { return rankingsTickMsg{} })
}

func gradTick() tea.Cmd {
	return tea.Tick(120*time.Millisecond, func(time.Time) tea.Msg { return gradTickMsg{} })
}

type App struct {
	client *api.Client

	width, height int
	tab           int
	loading       bool
	showHelp      bool
	spin          spinner.Model
	prog          progress.Model
	phase         int
	flash         string
	err           error

	models      []api.Model
	providers   []api.Provider
	rankings    *api.Rankings
	rankingsErr error
	rankingsSet bool
	bench       *api.Benchmarks
	benchErr    error
	benchSet    bool

	modelsView    ModelsView
	rankingsView  RankingsView
	benchView     BenchView
	providersView ProvidersView
	statsView     StatsView
	detail        *DetailView
}

func NewApp(client *api.Client) *App {
	sp := spinner.New(spinner.WithSpinner(spinner.MiniDot))
	sp.Style = lipgloss.NewStyle().Foreground(cPrimary)
	pg := progress.New(progress.WithDefaultGradient(), progress.WithWidth(44))
	return &App{client: client, spin: sp, prog: pg, loading: true}
}

func Run(client *api.Client) error {
	p := tea.NewProgram(NewApp(client), tea.WithAltScreen(), tea.WithMouseCellMotion())
	_, err := p.Run()
	return err
}

func (a *App) loadData(force bool) tea.Cmd {
	return func() tea.Msg {
		models, err := a.client.Models(force)
		if err != nil {
			return dataMsg{err: err}
		}
		providers, err := a.client.Providers(force)
		if err != nil {
			return dataMsg{err: err}
		}
		return dataMsg{models: models, providers: providers}
	}
}

func (a *App) fetchEndpoints(id string) tea.Cmd {
	return func() tea.Msg {
		eps, err := a.client.Endpoints(id)
		return endpointsMsg{id: id, data: eps, err: err}
	}
}

func (a *App) loadRankings(force bool) tea.Cmd {
	return func() tea.Msg {
		r, err := a.client.Rankings(force)
		return rankingsMsg{data: r, err: err}
	}
}

func (a *App) loadBench(force bool) tea.Cmd {
	return func() tea.Msg {
		b, err := a.client.Benchmarks(force)
		return benchMsg{data: b, err: err}
	}
}

func (a *App) Init() tea.Cmd {
	// rankings are always fetched fresh on open, then re-fetched every RankingsTTL
	return tea.Batch(a.spin.Tick, a.loadData(false), a.loadRankings(true), a.loadBench(false),
		rankingsTick(), gradTick(), a.prog.SetPercent(0.25))
}

func (a *App) contentSize() (int, int) {
	// logo line + 3-row tabs + status bar + key hints
	h := a.height - 6
	if h < 3 {
		h = 3
	}
	return a.width, h
}

func (a *App) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		a.width, a.height = msg.Width, msg.Height
		w, h := a.contentSize()
		a.modelsView.SetSize(w, h)
		a.rankingsView.SetSize(w, h)
		a.benchView.SetSize(w, h)
		a.providersView.SetSize(w, h)
		a.statsView.SetSize(w, h)
		if a.detail != nil {
			a.detail.SetSize(w, h)
		}
		return a, nil

	case dataMsg:
		if msg.err != nil {
			a.loading = false
			a.err = msg.err
			return a, nil
		}
		a.err = nil
		a.models, a.providers = msg.models, msg.providers
		w, h := a.contentSize()
		a.modelsView = NewModelsView(a.models, w, h)
		a.rankingsView = NewRankingsView(w, h)
		a.benchView = NewBenchView(w, h)
		a.providersView = NewProvidersView(a.providers, w, h)
		a.statsView = NewStatsView(api.ComputeStats(a.models, a.providers), w, h)
		if a.rankingsSet {
			a.rankingsView.SetData(a.rankings, a.rankingsErr)
		}
		if a.benchSet {
			a.benchView.SetData(a.bench, a.benchErr)
		}
		// let the progress bar finish its sweep before dropping the splash
		return a, tea.Batch(a.prog.SetPercent(1.0),
			tea.Tick(350*time.Millisecond, func(time.Time) tea.Msg { return splashDoneMsg{} }))

	case splashDoneMsg:
		a.loading = false
		return a, nil

	case rankingsMsg:
		a.rankings, a.rankingsErr, a.rankingsSet = msg.data, msg.err, true
		a.rankingsView.SetData(msg.data, msg.err)
		return a, nil

	case benchMsg:
		a.bench, a.benchErr, a.benchSet = msg.data, msg.err, true
		a.benchView.SetData(msg.data, msg.err)
		if a.detail != nil && msg.data != nil {
			a.detail.SetBench(msg.data.ForModel(a.detail.model))
		}
		return a, nil

	case rankingsTickMsg:
		return a, tea.Batch(a.loadRankings(true), rankingsTick())

	case gradTickMsg:
		if a.loading {
			a.phase++
			return a, gradTick()
		}
		return a, nil

	case flashClearMsg:
		a.flash = ""
		return a, nil

	case progress.FrameMsg:
		pm, cmd := a.prog.Update(msg)
		a.prog = pm.(progress.Model)
		return a, cmd

	case endpointsMsg:
		if a.detail != nil && a.detail.model.ID == msg.id {
			a.detail.SetEndpoints(msg.data, msg.err)
		}
		return a, nil

	case spinner.TickMsg:
		var cmd tea.Cmd
		a.spin, cmd = a.spin.Update(msg)
		if a.loading || (a.detail != nil && a.detail.loading) {
			return a, cmd
		}
		return a, nil

	case tea.MouseMsg:
		return a, a.handleMouse(msg)

	case tea.KeyMsg:
		return a.handleKey(msg)
	}
	return a, nil
}

func (a *App) handleMouse(msg tea.MouseMsg) tea.Cmd {
	if a.loading || a.showHelp {
		return nil
	}
	if a.detail != nil {
		return a.detail.Update(msg)
	}
	switch a.tab {
	case tabModels:
		a.modelsView.HandleMouse(msg)
		return nil
	case tabRankings:
		return a.rankingsView.Update(msg)
	case tabBench:
		return a.benchView.Update(msg)
	case tabProviders:
		a.providersView.HandleMouse(msg)
		return nil
	case tabStats:
		return a.statsView.Update(msg)
	}
	return nil
}

// selectedModelID is the model under the cursor, or the open detail's model.
func (a *App) selectedModelID() string {
	if a.detail != nil {
		return a.detail.model.ID
	}
	if a.tab == tabModels {
		if m := a.modelsView.Selected(); m != nil {
			return m.ID
		}
	}
	return ""
}

func (a *App) setFlash(s string) tea.Cmd {
	a.flash = s
	return tea.Tick(1500*time.Millisecond, func(time.Time) tea.Msg { return flashClearMsg{} })
}

func (a *App) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if msg.String() == "ctrl+c" {
		return a, tea.Quit
	}

	// help overlay swallows everything
	if a.showHelp {
		a.showHelp = false
		return a, nil
	}

	// detail overlay
	if a.detail != nil {
		switch msg.String() {
		case "esc", "q", "backspace":
			a.detail = nil
			return a, nil
		case "?":
			a.showHelp = true
			return a, nil
		case "o":
			openModelPage(a.detail.model.ID)
			return a, a.setFlash("opened " + a.detail.model.ID)
		case "y":
			return a, a.copyToClipboard(a.detail.model.ID)
		default:
			cmd := a.detail.Update(msg)
			return a, cmd
		}
	}

	// search input gets priority when focused
	if a.tab == tabModels && a.modelsView.Searching() {
		cmd := a.modelsView.Update(msg)
		return a, cmd
	}

	switch msg.String() {
	case "q":
		return a, tea.Quit
	case "?":
		a.showHelp = true
		return a, nil
	case "1":
		a.tab = tabModels
		return a, nil
	case "2":
		a.tab = tabRankings
		return a, nil
	case "3":
		a.tab = tabBench
		return a, nil
	case "4":
		a.tab = tabProviders
		return a, nil
	case "5":
		a.tab = tabStats
		return a, nil
	case "tab", "right", "l", "]":
		a.tab = (a.tab + 1) % tabCount
		return a, nil
	case "shift+tab", "left", "h", "[":
		a.tab = (a.tab + tabCount - 1) % tabCount
		return a, nil
	case "R":
		a.loading = true
		return a, tea.Batch(a.spin.Tick, gradTick(), a.prog.SetPercent(0.25),
			a.loadData(true), a.loadRankings(true), a.loadBench(true))
	case "o":
		if id := a.selectedModelID(); id != "" {
			openModelPage(id)
			return a, a.setFlash("opened " + id)
		}
	case "y":
		if id := a.selectedModelID(); id != "" {
			return a, a.copyToClipboard(id)
		}
	}

	switch a.tab {
	case tabModels:
		if msg.String() == "enter" {
			if m := a.modelsView.Selected(); m != nil {
				w, h := a.contentSize()
				a.detail = NewDetailView(*m, w, h)
				if a.bench != nil {
					a.detail.SetBench(a.bench.ForModel(*m))
				}
				return a, tea.Batch(a.spin.Tick, a.fetchEndpoints(m.ID))
			}
			return a, nil
		}
		cmd := a.modelsView.Update(msg)
		return a, cmd
	case tabRankings:
		cmd := a.rankingsView.Update(msg)
		return a, cmd
	case tabBench:
		cmd := a.benchView.Update(msg)
		return a, cmd
	case tabProviders:
		cmd := a.providersView.Update(msg)
		return a, cmd
	case tabStats:
		cmd := a.statsView.Update(msg)
		return a, cmd
	}
	return a, nil
}

func (a *App) copyToClipboard(text string) tea.Cmd {
	if err := copyText(text); err != nil {
		return a.setFlash("copy failed: no clipboard tool")
	}
	return a.setFlash("copied " + text)
}

func openModelPage(id string) {
	url := "https://openrouter.ai/" + strings.TrimPrefix(id, "~")
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	_ = cmd.Start()
}

func copyText(s string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("pbcopy")
	case "windows":
		cmd = exec.Command("clip")
	default:
		cmd = exec.Command("xclip", "-selection", "clipboard")
	}
	in, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	_, _ = in.Write([]byte(s))
	_ = in.Close()
	return cmd.Wait()
}

func (a *App) header() string {
	logo := gradient("◆ modelrouter", a.phase)
	tagline := styleTagline.Render("  the OpenRouter explorer")
	count := ""
	if len(a.models) > 0 {
		count = styleDim.Render(fmt.Sprintf("%d models · %d providers", len(a.models), len(a.providers)))
	}
	gap := a.width - lipgloss.Width(logo) - lipgloss.Width(tagline) - lipgloss.Width(count) - 2
	if gap < 1 {
		gap = 1
	}
	top := " " + logo + tagline + strings.Repeat(" ", gap) + count
	return top + "\n" + renderTabs(a.width, a.tab, tabNames)
}

// statusBar renders mode + context info segments with a right-aligned readout.
func (a *App) statusBar() string {
	mode := tabNames[a.tab]
	if a.detail != nil {
		mode = "Detail"
	}
	seg := styleSegMode.Render(strings.ToUpper(mode))

	var info string
	switch {
	case a.detail != nil:
		info = "models › " + a.detail.model.ID
	case a.tab == tabModels:
		cur, total := a.modelsView.CursorInfo()
		info = fmt.Sprintf("model %d/%d · sort %s", cur, total, a.modelsView.SortLabel())
	case a.tab == tabRankings:
		if a.rankings != nil {
			info = "live leaderboards · updated " + a.rankings.FetchedAt.Format("15:04:05")
		} else {
			info = "loading leaderboards..."
		}
	case a.tab == tabBench:
		info = "Artificial Analysis + Design Arena"
	case a.tab == tabProviders:
		info = fmt.Sprintf("%d inference providers", len(a.providers))
	default:
		info = "catalog analytics"
	}
	seg += styleSegInfo.Render(info)

	var right string
	if a.flash != "" {
		right = styleSegFlash.Render("✓ " + a.flash)
	} else {
		switch {
		case a.detail != nil:
			right = styleSegInfo.Render(fmt.Sprintf("%3.0f%%", a.detail.ScrollPercent()*100))
		case a.tab == tabRankings:
			right = styleSegInfo.Render(fmt.Sprintf("%3.0f%% · refresh 5m", a.rankingsView.ScrollPercent()*100))
		case a.tab == tabBench:
			right = styleSegInfo.Render(fmt.Sprintf("%3.0f%%", a.benchView.ScrollPercent()*100))
		case a.tab == tabStats:
			right = styleSegInfo.Render(fmt.Sprintf("%3.0f%%", a.statsView.ScrollPercent()*100))
		default:
			right = styleSegInfo.Render("? help")
		}
	}

	fill := a.width - lipgloss.Width(seg) - lipgloss.Width(right)
	if fill < 0 {
		fill = 0
	}
	return seg + styleSegFill.Render(strings.Repeat(" ", fill)) + right
}

func (a *App) hints() string {
	if a.detail != nil {
		return footerHelp("↑↓/wheel", "scroll", "g G", "top/bottom", "o", "open site", "y", "copy id", "esc", "back", "?", "help")
	}
	switch a.tab {
	case tabModels:
		return footerHelp("enter", "details", "/", "search", "s", "sort", "f t m", "filters", "o", "open", "y", "copy", "?", "help", "q", "quit")
	case tabRankings:
		return footerHelp("↑↓/wheel", "scroll", "p", "apps period", "h l", "tabs", "R", "refresh", "?", "help", "q", "quit")
	case tabProviders:
		return footerHelp("↑↓/wheel", "move", "h l", "tabs", "R", "refresh", "?", "help", "q", "quit")
	default:
		return footerHelp("↑↓/wheel", "scroll", "h l", "tabs", "R", "refresh", "?", "help", "q", "quit")
	}
}

func (a *App) View() string {
	if a.width == 0 {
		return ""
	}
	if a.loading {
		box := lipgloss.NewStyle().Padding(1, 4).
			Border(lipgloss.RoundedBorder()).BorderForeground(cPrimary).
			Render(gradient("◆ modelrouter", a.phase) + styleTagline.Render("  the OpenRouter explorer") +
				"\n\n" + a.prog.View() +
				"\n\n" + a.spin.View() + " " + styleDim.Render("fetching catalog + live rankings..."))
		return lipgloss.Place(a.width, a.height, lipgloss.Center, lipgloss.Center, box)
	}
	if a.err != nil {
		box := styleErr.Render("error: ") + a.err.Error() + "\n\n" + styleDim.Render("R to retry · q to quit")
		return lipgloss.Place(a.width, a.height, lipgloss.Center, lipgloss.Center, box)
	}

	_, h := a.contentSize()
	var content string
	switch {
	case a.showHelp:
		content = lipgloss.Place(a.width, h, lipgloss.Center, lipgloss.Center, helpOverlay())
	case a.detail != nil:
		content = a.detail.View(a.spin)
	default:
		switch a.tab {
		case tabModels:
			content = a.modelsView.View()
		case tabRankings:
			content = a.rankingsView.View()
		case tabBench:
			content = a.benchView.View()
		case tabProviders:
			content = a.providersView.View()
		case tabStats:
			content = a.statsView.View()
		}
	}
	content = lipgloss.NewStyle().Height(h).MaxHeight(h).Render(content)
	return a.header() + "\n" + content + "\n" + a.statusBar() + "\n" + a.hints()
}
