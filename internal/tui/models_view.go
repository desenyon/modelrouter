package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/table"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/desenyon/modelrouter/internal/api"
)

var modalityCycle = []string{"", "image", "audio", "file"}

type ModelsView struct {
	all      []api.Model
	filtered []api.Model

	tbl      table.Model
	search   textinput.Model
	sortKey  api.SortKey
	sortDesc bool
	filter   api.Filter
	modIdx   int

	width, height int
}

func NewModelsView(models []api.Model, w, h int) ModelsView {
	ti := textinput.New()
	ti.Placeholder = "fuzzy search models..."
	ti.Prompt = "/ "
	ti.PromptStyle = lipgloss.NewStyle().Foreground(cPrimary).Bold(true)
	ti.CharLimit = 64

	tbl := table.New(table.WithFocused(true))
	st := table.DefaultStyles()
	st.Header = st.Header.Bold(true).Foreground(cPrimary).
		BorderStyle(lipgloss.NormalBorder()).BorderForeground(cFaint).BorderBottom(true)
	st.Selected = lipgloss.NewStyle().Foreground(lipgloss.Color("#FFFFFF")).Background(cPrimary).Bold(true)
	tbl.SetStyles(st)

	v := ModelsView{all: models, tbl: tbl, search: ti, sortKey: api.SortNewest}
	v.SetSize(w, h)
	v.refresh()
	return v
}

func (v *ModelsView) SetSize(w, h int) {
	v.width, v.height = w, h
	if h < 4 {
		h = 4
	}
	v.tbl.SetHeight(h - 3) // status line + table header rows
	v.tbl.SetColumns(v.columns())
	v.refresh()
}

func (v *ModelsView) columns() []table.Column {
	fixed := 7 + 9 + 9 + 8 + 5 + 6 // ctx,in,out,mods,caps,age
	idW := v.width - fixed - 14    // cell padding margin
	if idW < 24 {
		idW = 24
	}
	if idW > 62 {
		idW = 62
	}
	return []table.Column{
		{Title: "MODEL", Width: idW},
		{Title: "CTX", Width: 7},
		{Title: "IN $/M", Width: 9},
		{Title: "OUT $/M", Width: 9},
		{Title: "MODS", Width: 8},
		{Title: "CAPS", Width: 5},
		{Title: "AGE", Width: 6},
	}
}

func (v *ModelsView) refresh() {
	v.filter.Query = v.search.Value()
	v.filtered = api.FilterModels(v.all, v.filter)
	if v.filter.Query == "" { // fuzzy already ranks when searching
		api.SortModels(v.filtered, v.sortKey, v.sortDesc)
	}
	rows := make([]table.Row, len(v.filtered))
	for i, m := range v.filtered {
		in := api.FmtPrice(m.Pricing.PromptPerM())
		out := api.FmtPrice(m.Pricing.CompletionPerM())
		rows[i] = table.Row{
			m.ID,
			api.FmtCtx(m.ContextLength),
			in,
			out,
			api.FmtModalities(m.Architecture),
			api.FmtCaps(m),
			api.FmtAge(m.CreatedTime()),
		}
	}
	v.tbl.SetRows(rows)
	if v.tbl.Cursor() >= len(rows) {
		v.tbl.SetCursor(0)
	}
}

func (v *ModelsView) Searching() bool { return v.search.Focused() }

// HandleMouse maps wheel events to table cursor movement.
func (v *ModelsView) HandleMouse(msg tea.MouseMsg) {
	if msg.Action != tea.MouseActionPress {
		return
	}
	switch msg.Button {
	case tea.MouseButtonWheelUp:
		v.tbl.MoveUp(2)
	case tea.MouseButtonWheelDown:
		v.tbl.MoveDown(2)
	}
}

func (v *ModelsView) CursorInfo() (int, int) {
	if len(v.filtered) == 0 {
		return 0, 0
	}
	return v.tbl.Cursor() + 1, len(v.filtered)
}

func (v *ModelsView) SortLabel() string {
	dir := "↓"
	if v.sortDesc {
		dir = "↑"
	}
	return v.sortKey.String() + " " + dir
}

func (v *ModelsView) Selected() *api.Model {
	i := v.tbl.Cursor()
	if i < 0 || i >= len(v.filtered) {
		return nil
	}
	return &v.filtered[i]
}

func (v *ModelsView) Update(msg tea.Msg) tea.Cmd {
	if key, ok := msg.(tea.KeyMsg); ok {
		if v.search.Focused() {
			switch key.String() {
			case "enter", "esc":
				if key.String() == "esc" {
					v.search.SetValue("")
				}
				v.search.Blur()
				v.tbl.Focus()
				v.refresh()
				return nil
			case "ctrl+c":
				return tea.Quit
			default:
				var cmd tea.Cmd
				v.search, cmd = v.search.Update(msg)
				v.refresh()
				return cmd
			}
		}
		switch key.String() {
		case "/":
			v.tbl.Blur()
			return v.search.Focus()
		case "s":
			v.sortKey = v.sortKey.Next()
			v.refresh()
			return nil
		case "v":
			v.sortDesc = !v.sortDesc
			v.refresh()
			return nil
		case "f":
			v.filter.FreeOnly = !v.filter.FreeOnly
			v.refresh()
			return nil
		case "t":
			v.filter.Tools = !v.filter.Tools
			v.refresh()
			return nil
		case "m":
			v.modIdx = (v.modIdx + 1) % len(modalityCycle)
			v.filter.Modality = modalityCycle[v.modIdx]
			v.refresh()
			return nil
		case "esc":
			if v.filter.Active() {
				v.search.SetValue("")
				v.filter = api.Filter{}
				v.modIdx = 0
				v.refresh()
				return nil
			}
		}
	}
	var cmd tea.Cmd
	v.tbl, cmd = v.tbl.Update(msg)
	return cmd
}

func (v *ModelsView) statusLine() string {
	var chips []string
	dir := "↓"
	if v.sortDesc {
		dir = "↑"
	}
	if v.search.Focused() {
		chips = append(chips, v.search.View())
	} else {
		if q := v.search.Value(); q != "" {
			chips = append(chips, styleChip.Render("/"+q))
		}
		chips = append(chips, styleChipOff.Render("sort: "+v.sortKey.String()+" "+dir))
	}
	if v.filter.FreeOnly {
		chips = append(chips, styleChip.Render("free"))
	}
	if v.filter.Tools {
		chips = append(chips, styleChip.Render("tools"))
	}
	if v.filter.Modality != "" {
		chips = append(chips, styleChip.Render("input: "+v.filter.Modality))
	}
	count := styleDim.Render(fmt.Sprintf("%d/%d", len(v.filtered), len(v.all)))
	line := " " + strings.Join(chips, "")
	gap := v.width - lipgloss.Width(line) - lipgloss.Width(count) - 2
	if gap < 1 {
		gap = 1
	}
	return line + strings.Repeat(" ", gap) + count
}

func (v *ModelsView) View() string {
	if len(v.filtered) == 0 {
		empty := lipgloss.Place(v.width, v.height-2, lipgloss.Center, lipgloss.Center,
			styleDim.Render("no models match — esc to clear filters"))
		return v.statusLine() + "\n" + empty
	}
	return v.statusLine() + "\n" + v.tbl.View()
}
