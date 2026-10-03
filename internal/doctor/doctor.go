// Package doctor verifies that the catalog matches what each provider
// actually serves: credentials work, every catalog model id exists upstream,
// and (optionally) each model answers a tiny live request.
package doctor

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/desenyon/modelrouter/internal/canon"
	"github.com/desenyon/modelrouter/internal/catalog"
	"github.com/desenyon/modelrouter/internal/config"
	"github.com/desenyon/modelrouter/internal/provider"
)

// ModelCheck is the result for one catalog model.
type ModelCheck struct {
	ID       string `json:"id"`
	Provider string `json:"provider"`
	Enabled  bool   `json:"auto_routed"`
	Listed   string `json:"listed"` // yes | no | unknown
	Probe    string `json:"probe,omitempty"`
	Detail   string `json:"detail,omitempty"`
}

// ProviderCheck is the result for one provider.
type ProviderCheck struct {
	Name       string `json:"name"`
	Configured bool   `json:"configured"`
	Auth       string `json:"auth"` // ok | failed | skipped
	Upstream   int    `json:"upstream_models"`
	Detail     string `json:"detail,omitempty"`
}

// Report is the doctor output.
type Report struct {
	Providers []ProviderCheck `json:"providers"`
	Models    []ModelCheck    `json:"models"`
	Problems  int             `json:"problems"`
}

// Run checks every native provider. probe sends one tiny request per
// auto-routed model (costs a fraction of a cent per model).
func Run(ctx context.Context, cfg config.Config, cat *catalog.Catalog, providers map[string]provider.Provider, probe bool) Report {
	var rep Report
	hc := &http.Client{Timeout: 20 * time.Second}
	listed := map[string]map[string]bool{}
	type src struct {
		name string
		key  string
		list func(context.Context, *http.Client, string, string) (map[string]bool, error)
		base string
	}
	p := cfg.Providers
	for _, sc := range []src{
		{"openai", p.OpenAI.APIKey, listOpenAI, p.OpenAI.BaseURL},
		{"anthropic", p.Anthropic.APIKey, listAnthropic, p.Anthropic.BaseURL},
		{"gemini", p.Gemini.APIKey, listGemini, p.Gemini.BaseURL},
	} {
		pc := ProviderCheck{Name: sc.name, Configured: sc.key != ""}
		if sc.key == "" {
			pc.Auth = "skipped"
			pc.Detail = "no API key (set " + map[string]string{"openai": "OPENAI_API_KEY", "anthropic": "ANTHROPIC_API_KEY", "gemini": "GEMINI_API_KEY"}[sc.name] + ")"
		} else if ids, err := sc.list(ctx, hc, sc.base, sc.key); err != nil {
			pc.Auth, pc.Detail = "failed", err.Error()
			rep.Problems++
		} else {
			pc.Auth, pc.Upstream = "ok", len(ids)
			listed[sc.name] = ids
		}
		rep.Providers = append(rep.Providers, pc)
	}
	for _, m := range cat.All() {
		mc := ModelCheck{ID: m.ID, Provider: m.Provider, Enabled: m.Enabled, Listed: "unknown"}
		if ids, ok := listed[m.Provider]; ok {
			if ids[m.UpstreamID] {
				mc.Listed = "yes"
			} else {
				mc.Listed = "no"
				mc.Detail = "upstream id " + m.UpstreamID + " not in provider model list"
				if m.Enabled {
					rep.Problems++
				}
			}
		}
		if probe && m.Enabled {
			if prov, ok := providers[m.Provider]; ok {
				mc.Probe, mc.Detail = probeModel(ctx, prov, m)
				if mc.Probe != "ok" {
					rep.Problems++
				}
			}
		}
		rep.Models = append(rep.Models, mc)
	}
	return rep
}

func probeModel(ctx context.Context, prov provider.Provider, m *catalog.Model) (string, string) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	effort := ""
	if len(m.Efforts) > 0 {
		effort = m.Efforts[0]
	}
	req := &canon.Request{Messages: []canon.Message{{Role: canon.RoleUser, Parts: []canon.Part{{Type: canon.PartText, Text: "Reply with the single word: ok"}}}}}
	maxTok := 2048
	if m.MaxOutput < maxTok {
		maxTok = m.MaxOutput
	}
	start := time.Now()
	st, err := prov.Open(ctx, &provider.Call{Req: req, Model: m, UpstreamID: m.UpstreamID, Effort: effort, MaxTokens: maxTok})
	if err != nil {
		return "failed", err.Error()
	}
	defer st.Close()
	var asm canon.Assembler
	for {
		ev, err := st.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "failed", err.Error()
		}
		asm.Add(ev)
	}
	r := asm.Response()
	return "ok", fmt.Sprintf("%q in %s (in=%d out=%d)", trunc(strings.TrimSpace(r.Content), 30), time.Since(start).Round(time.Millisecond), r.Usage.InputTokens, r.Usage.OutputTokens)
}

func getJSON(ctx context.Context, hc *http.Client, u string, hdr map[string]string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	for k, val := range hdr {
		req.Header.Set(k, val)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		e := provider.FromHTTP("", "", resp)
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, e.Message)
	}
	return json.NewDecoder(resp.Body).Decode(v)
}

func listOpenAI(ctx context.Context, hc *http.Client, base, key string) (map[string]bool, error) {
	if base == "" {
		base = "https://api.openai.com/v1"
	}
	var out struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := getJSON(ctx, hc, strings.TrimRight(base, "/")+"/models", map[string]string{"Authorization": "Bearer " + key}, &out); err != nil {
		return nil, err
	}
	ids := map[string]bool{}
	for _, d := range out.Data {
		ids[d.ID] = true
	}
	return ids, nil
}

func listAnthropic(ctx context.Context, hc *http.Client, base, key string) (map[string]bool, error) {
	if base == "" {
		base = "https://api.anthropic.com"
	}
	ids := map[string]bool{}
	after := ""
	for page := 0; page < 20; page++ {
		u := strings.TrimRight(base, "/") + "/v1/models?limit=1000"
		if after != "" {
			u += "&after_id=" + url.QueryEscape(after)
		}
		var out struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
			HasMore bool   `json:"has_more"`
			LastID  string `json:"last_id"`
		}
		if err := getJSON(ctx, hc, u, map[string]string{"x-api-key": key, "anthropic-version": "2023-06-01"}, &out); err != nil {
			return nil, err
		}
		for _, d := range out.Data {
			ids[d.ID] = true
		}
		if !out.HasMore || out.LastID == "" {
			break
		}
		after = out.LastID
	}
	return ids, nil
}

func listGemini(ctx context.Context, hc *http.Client, base, key string) (map[string]bool, error) {
	if base == "" {
		base = "https://generativelanguage.googleapis.com"
	}
	ids := map[string]bool{}
	token := ""
	for page := 0; page < 20; page++ {
		u := strings.TrimRight(base, "/") + "/v1beta/models?pageSize=1000"
		if token != "" {
			u += "&pageToken=" + url.QueryEscape(token)
		}
		var out struct {
			Models []struct {
				Name string `json:"name"`
			} `json:"models"`
			NextPageToken string `json:"nextPageToken"`
		}
		if err := getJSON(ctx, hc, u, map[string]string{"x-goog-api-key": key}, &out); err != nil {
			return nil, err
		}
		for _, m := range out.Models {
			ids[strings.TrimPrefix(m.Name, "models/")] = true
		}
		if out.NextPageToken == "" {
			break
		}
		token = out.NextPageToken
	}
	return ids, nil
}

// Print renders a report.
func Print(w io.Writer, r Report) {
	fmt.Fprintln(w, "providers")
	for _, p := range r.Providers {
		fmt.Fprintf(w, "  %-10s auth=%-8s models=%-4d %s\n", p.Name, p.Auth, p.Upstream, p.Detail)
	}
	fmt.Fprintln(w, "\ncatalog models")
	ms := append([]ModelCheck(nil), r.Models...)
	sort.SliceStable(ms, func(i, j int) bool { return ms[i].Provider < ms[j].Provider })
	for _, m := range ms {
		auto := " "
		if m.Enabled {
			auto = "*"
		}
		probe := ""
		if m.Probe != "" {
			probe = " probe=" + m.Probe
		}
		fmt.Fprintf(w, "  %s %-34s listed=%-7s%s %s\n", auto, m.ID, m.Listed, probe, m.Detail)
	}
	fmt.Fprintf(w, "\n(* = auto-routed)  problems: %d\n", r.Problems)
}

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
