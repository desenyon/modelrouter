package api

import (
	"sort"
	"strings"

	"github.com/sahilm/fuzzy"
)

type SortKey int

const (
	SortNewest SortKey = iota
	SortName
	SortPrompt
	SortCompletion
	SortContext
	sortKeyCount
)

func (k SortKey) String() string {
	switch k {
	case SortNewest:
		return "newest"
	case SortName:
		return "name"
	case SortPrompt:
		return "prompt $"
	case SortCompletion:
		return "completion $"
	case SortContext:
		return "context"
	}
	return "?"
}

func (k SortKey) Next() SortKey { return (k + 1) % sortKeyCount }

func ParseSortKey(s string) SortKey {
	switch strings.ToLower(s) {
	case "name":
		return SortName
	case "prompt", "input":
		return SortPrompt
	case "completion", "output":
		return SortCompletion
	case "context", "ctx":
		return SortContext
	default:
		return SortNewest
	}
}

func SortModels(ms []Model, key SortKey, desc bool) {
	less := func(a, b Model) bool { return a.Created > b.Created } // newest first by default
	switch key {
	case SortName:
		less = func(a, b Model) bool { return strings.ToLower(a.ID) < strings.ToLower(b.ID) }
	case SortPrompt:
		less = func(a, b Model) bool { return a.Pricing.PromptPerM() < b.Pricing.PromptPerM() }
	case SortCompletion:
		less = func(a, b Model) bool { return a.Pricing.CompletionPerM() < b.Pricing.CompletionPerM() }
	case SortContext:
		less = func(a, b Model) bool { return a.ContextLength > b.ContextLength } // big ctx first
	}
	sort.SliceStable(ms, func(i, j int) bool {
		if desc {
			return less(ms[j], ms[i])
		}
		return less(ms[i], ms[j])
	})
}

type Filter struct {
	Query    string
	FreeOnly bool
	Tools    bool
	Modality string // "" = any; otherwise an input modality: image, audio, file, text
}

func (f Filter) Active() bool {
	return f.Query != "" || f.FreeOnly || f.Tools || f.Modality != ""
}

// FilterModels narrows ms; the fuzzy query also ranks results by match score.
func FilterModels(ms []Model, f Filter) []Model {
	out := make([]Model, 0, len(ms))
	for _, m := range ms {
		if f.FreeOnly && !m.IsFree() {
			continue
		}
		if f.Tools && !m.HasParam("tools") {
			continue
		}
		if f.Modality != "" && !m.HasInput(f.Modality) {
			continue
		}
		out = append(out, m)
	}
	if q := strings.TrimSpace(f.Query); q != "" {
		targets := make([]string, len(out))
		for i, m := range out {
			targets[i] = m.ID + " " + m.Name
		}
		matches := fuzzy.Find(q, targets)
		ranked := make([]Model, len(matches))
		for i, mt := range matches {
			ranked[i] = out[mt.Index]
		}
		return ranked
	}
	return out
}

func FindModel(ms []Model, id string) *Model {
	for i := range ms {
		if ms[i].ID == id || ms[i].CanonicalSlug == id {
			return &ms[i]
		}
	}
	// fall back to best fuzzy match so `modelrouter model fable` works
	targets := make([]string, len(ms))
	for i, m := range ms {
		targets[i] = m.ID
	}
	if matches := fuzzy.Find(id, targets); len(matches) > 0 {
		return &ms[matches[0].Index]
	}
	return nil
}
