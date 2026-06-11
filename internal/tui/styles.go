package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

var (
	cPrimary = lipgloss.Color("#7D56F4") // violet
	cAccent  = lipgloss.Color("#04B575") // green
	cPink    = lipgloss.Color("#FF6AC1")
	cCyan    = lipgloss.Color("#41D6E0")
	cYellow  = lipgloss.Color("#F2C744")
	cRed     = lipgloss.Color("#FF5F87")
	cDim     = lipgloss.AdaptiveColor{Light: "#9A9A9A", Dark: "#626262"}
	cFaint   = lipgloss.AdaptiveColor{Light: "#B5B5B5", Dark: "#3A3A3A"}
	cText    = lipgloss.AdaptiveColor{Light: "#1A1A1A", Dark: "#DDDDDD"}
	cBarBg   = lipgloss.AdaptiveColor{Light: "#ECECEC", Dark: "#1C1C28"}

	logoHues = []lipgloss.Color{
		"#FF6AC1", "#E963D6", "#C95FE8", "#A55BF0", "#7D56F4",
		"#5F6AF5", "#418CF0", "#41B6E0", "#41D6E0", "#2FD9B5", "#04B575",
	}

	styleTagline = lipgloss.NewStyle().Foreground(cDim).Italic(true)

	// tabs with joined borders (the classic lipgloss tab look)
	inactiveTabBorder = tabBorderWithBottom("┴", "─", "┴")
	activeTabBorder   = tabBorderWithBottom("┘", " ", "└")
	styleTabInactive  = lipgloss.NewStyle().
				Border(inactiveTabBorder, true).BorderForeground(cFaint).
				Foreground(cDim).Padding(0, 2)
	styleTabActive = styleTabInactive.
			Border(activeTabBorder, true).BorderForeground(cPrimary).
			Foreground(cPrimary).Bold(true)

	styleFooter  = lipgloss.NewStyle().Foreground(cDim)
	styleFootKey = lipgloss.NewStyle().Foreground(cText).Bold(true)

	// status bar segments
	styleSegMode = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#FFFFFF")).Background(cPrimary).Bold(true).Padding(0, 1)
	styleSegInfo  = lipgloss.NewStyle().Foreground(cDim).Background(cBarBg).Padding(0, 1)
	styleSegFlash = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#FFFFFF")).Background(cAccent).Bold(true).Padding(0, 1)
	styleSegFill = lipgloss.NewStyle().Background(cBarBg)

	styleSection = lipgloss.NewStyle().Foreground(cPrimary).Bold(true)
	styleDim     = lipgloss.NewStyle().Foreground(cDim)
	styleFree    = lipgloss.NewStyle().Foreground(cAccent).Bold(true)
	styleErr     = lipgloss.NewStyle().Foreground(cRed).Bold(true)

	styleChip = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#FFFFFF")).Background(cPrimary).
			Padding(0, 1).MarginRight(1)
	styleChipOff = lipgloss.NewStyle().
			Foreground(cDim).Background(cFaint).
			Padding(0, 1).MarginRight(1)

	styleBadge = lipgloss.NewStyle().
			Foreground(cText).Background(cFaint).Padding(0, 1)

	styleHelpPanel = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).BorderForeground(cPrimary).Padding(1, 3)
)

func tabBorderWithBottom(left, middle, right string) lipgloss.Border {
	border := lipgloss.RoundedBorder()
	border.BottomLeft = left
	border.Bottom = middle
	border.BottomRight = right
	return border
}

// renderTabs draws the tab row with a baseline extending to the right edge.
func renderTabs(width, active int, names []string) string {
	var tabs []string
	for i, name := range names {
		label := fmt.Sprintf("%d %s", i+1, name)
		if i == active {
			tabs = append(tabs, styleTabActive.Render(label))
		} else {
			tabs = append(tabs, styleTabInactive.Render(label))
		}
	}
	row := lipgloss.JoinHorizontal(lipgloss.Bottom, tabs...)
	gap := width - lipgloss.Width(row)
	if gap > 0 {
		filler := strings.Repeat(" ", gap-1) + "\n" + strings.Repeat(" ", gap-1) + "\n" +
			strings.Repeat("─", gap-1)
		row = lipgloss.JoinHorizontal(lipgloss.Bottom, row,
			lipgloss.NewStyle().Foreground(cFaint).Render(filler))
	}
	return row
}

// gradient colors each rune across the logo palette; phase animates the hues.
func gradient(s string, phase int) string {
	runes := []rune(s)
	if len(runes) == 0 {
		return s
	}
	var b strings.Builder
	n := len(logoHues)
	for i, r := range runes {
		hue := logoHues[(i*n/len(runes)+phase)%n]
		b.WriteString(lipgloss.NewStyle().Foreground(hue).Bold(true).Render(string(r)))
	}
	return b.String()
}

// bar renders a horizontal bar chart row scaled to maxVal within width cells.
func bar(count, maxVal, width int, color lipgloss.Color) string {
	if maxVal <= 0 {
		maxVal = 1
	}
	filled := count * width / maxVal
	if count > 0 && filled == 0 {
		filled = 1
	}
	return lipgloss.NewStyle().Foreground(color).Render(strings.Repeat("█", filled)) +
		lipgloss.NewStyle().Foreground(cFaint).Render(strings.Repeat("░", width-filled))
}

func uptimeStyle(pct float64) lipgloss.Style {
	switch {
	case pct >= 99:
		return lipgloss.NewStyle().Foreground(cAccent)
	case pct >= 95:
		return lipgloss.NewStyle().Foreground(cYellow)
	default:
		return lipgloss.NewStyle().Foreground(cRed)
	}
}

func footerHelp(pairs ...string) string {
	var parts []string
	for i := 0; i+1 < len(pairs); i += 2 {
		parts = append(parts, styleFootKey.Render(pairs[i])+styleFooter.Render(" "+pairs[i+1]))
	}
	return styleFooter.Render(" ") + strings.Join(parts, styleFooter.Render("  ·  "))
}

// helpOverlay is the full keymap panel toggled with "?".
func helpOverlay() string {
	section := func(title string, rows ...[2]string) string {
		var b strings.Builder
		b.WriteString(styleSection.Render(title) + "\n")
		for _, r := range rows {
			b.WriteString(fmt.Sprintf("  %s %s\n",
				styleFootKey.Width(14).Render(r[0]), styleFooter.Render(r[1])))
		}
		return b.String()
	}
	left := section("NAVIGATION",
		[2]string{"1-5", "jump to tab"},
		[2]string{"tab / h l", "next / prev tab"},
		[2]string{"←  →", "prev / next tab"},
		[2]string{"↑↓ j k", "move / scroll"},
		[2]string{"g G", "top / bottom"},
		[2]string{"mouse wheel", "scroll anywhere"},
		[2]string{"enter", "open model details"},
		[2]string{"esc", "back / clear filters"},
	) + "\n" + section("ACTIONS",
		[2]string{"o", "open on openrouter.ai"},
		[2]string{"y", "copy model id"},
		[2]string{"R", "refresh all data"},
		[2]string{"q ctrl+c", "quit"},
	)
	right := section("MODELS TAB",
		[2]string{"/", "fuzzy search"},
		[2]string{"s", "cycle sort key"},
		[2]string{"v", "reverse sort"},
		[2]string{"f", "free models only"},
		[2]string{"t", "tool-calling only"},
		[2]string{"m", "cycle input modality"},
	) + "\n" + section("RANKINGS TAB",
		[2]string{"p", "cycle apps period"},
	)
	body := lipgloss.JoinHorizontal(lipgloss.Top,
		lipgloss.NewStyle().MarginRight(6).Render(left), right)
	title := gradient("◆ modelrouter", 0) + styleTagline.Render("  keymap") + "\n\n"
	return styleHelpPanel.Render(title + body + "\n" + styleDim.Render("press ? or esc to close"))
}
