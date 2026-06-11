package api

import (
	"fmt"
	"strings"
	"time"
)

// FmtPrice renders USD-per-million values compactly; zero is "free".
func FmtPrice(perM float64) string {
	switch {
	case perM == 0:
		return "free"
	case perM >= 100:
		return fmt.Sprintf("$%.0f", perM)
	case perM >= 1:
		return fmt.Sprintf("$%.2f", perM)
	case perM >= 0.01:
		return fmt.Sprintf("$%.3f", perM)
	default:
		return fmt.Sprintf("$%.4f", perM)
	}
}

func FmtCtx(n int) string {
	switch {
	case n >= 1_000_000:
		v := float64(n) / 1_000_000
		if v == float64(int(v)) {
			return fmt.Sprintf("%dM", int(v))
		}
		return fmt.Sprintf("%.1fM", v)
	case n >= 1000:
		return fmt.Sprintf("%dK", n/1000)
	case n == 0:
		return "-"
	default:
		return fmt.Sprintf("%d", n)
	}
}

func FmtAge(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	case d < 14*24*time.Hour:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	case d < 60*24*time.Hour:
		return fmt.Sprintf("%dw", int(d.Hours()/(24*7)))
	case d < 365*24*time.Hour:
		return fmt.Sprintf("%dmo", int(d.Hours()/(24*30)))
	default:
		return fmt.Sprintf("%.1fy", d.Hours()/(24*365))
	}
}

// FmtModalities renders e.g. "tif→t" for text+image+file in, text out.
func FmtModalities(a Architecture) string {
	short := func(mods []string) string {
		var b strings.Builder
		for _, m := range mods {
			if len(m) > 0 {
				b.WriteByte(m[0])
			}
		}
		if b.Len() == 0 {
			return "?"
		}
		return b.String()
	}
	return short(a.InputModalities) + "→" + short(a.OutputModalities)
}

// FmtCaps marks tool calling, reasoning, and structured outputs support.
func FmtCaps(m Model) string {
	caps := ""
	if m.HasParam("tools") {
		caps += "T"
	} else {
		caps += "·"
	}
	if m.HasParam("reasoning") || m.HasParam("include_reasoning") {
		caps += "R"
	} else {
		caps += "·"
	}
	if m.HasParam("structured_outputs") || m.HasParam("response_format") {
		caps += "S"
	} else {
		caps += "·"
	}
	return caps
}
