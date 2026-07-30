package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func testClient(t *testing.T, handler http.Handler) (*Client, *httptest.Server) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return &Client{
		http:            server.Client(),
		baseURL:         server.URL,
		frontendBaseURL: server.URL + "/frontend",
		CacheDir:        t.TempDir(),
		TTL:             time.Minute,
	}, server
}

func TestModelsRequestsAllOutputModalities(t *testing.T) {
	client, _ := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" || r.URL.Query().Get("output_modalities") != "all" {
			t.Fatalf("unexpected request: %s", r.URL.String())
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"acme/image","name":"Image","architecture":{"output_modalities":["image"]},"pricing":{"prompt":"0.01","image_output":"0.02"}}]}`))
	}))

	models, err := client.Models(true)
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 1 || !models[0].HasOutput("image") {
		t.Fatalf("unexpected models: %#v", models)
	}
	if got := models[0].OutputPrice(); got != "$20.0k/Mu" {
		t.Fatalf("unexpected image price %q", got)
	}
}

func TestProvidersMergeOfficialAndRichFeeds(t *testing.T) {
	client, _ := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/providers":
			_, _ = w.Write([]byte(`{"data":[{"name":"Acme","slug":"acme","headquarters":"US","datacenters":["us-east"],"privacy_policy_url":"https://official/privacy"}]}`))
		case "/frontend/providers":
			_, _ = w.Write([]byte(`[{"displayName":"Acme Cloud","slug":"acme","headquarters":"CA","hasChatCompletions":true,"byokEnabled":true,"dataPolicy":{"retainsPrompts":true,"requiresUserIDs":true,"termsOfServiceURL":"https://rich/terms"}},{"displayName":"Extra","slug":"extra"}]`))
		default:
			http.NotFound(w, r)
		}
	}))

	providers, err := client.Providers(true)
	if err != nil {
		t.Fatal(err)
	}
	if len(providers) != 2 {
		t.Fatalf("got %d providers", len(providers))
	}
	acme := providers[0]
	if acme.Label() != "Acme Cloud" || !acme.BYOKEnabled || acme.Headquarters != "CA" {
		t.Fatalf("rich fields not merged: %#v", acme)
	}
	if len(acme.Datacenters) != 1 || acme.PrivacyURL() != "https://official/privacy" || acme.TermsURL() != "https://rich/terms" {
		t.Fatalf("official/rich ownership not preserved: %#v", acme)
	}
}

func TestProvidersUseOfficialFallbackWhenRichFeedFails(t *testing.T) {
	client, _ := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/providers" {
			_, _ = w.Write([]byte(`{"data":[{"name":"Official","slug":"official"}]}`))
			return
		}
		http.Error(w, "down", http.StatusBadGateway)
	}))

	providers, err := client.Providers(true)
	if err != nil {
		t.Fatal(err)
	}
	if len(providers) != 1 || providers[0].Slug != "official" {
		t.Fatalf("unexpected fallback: %#v", providers)
	}
}

func TestFrontendGetAcceptsEnvelopeAndRawResponses(t *testing.T) {
	client, _ := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/frontend/enveloped":
			_, _ = w.Write([]byte(`{"data":[1,2]}`))
		case "/frontend/raw":
			_, _ = w.Write([]byte(`[3,4]`))
		default:
			http.NotFound(w, r)
		}
	}))

	enveloped, err := frontendGet[[]int](client, "env", "/enveloped", true)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := frontendGet[[]int](client, "raw", "/raw", true)
	if err != nil {
		t.Fatal(err)
	}
	if len(enveloped) != 2 || enveloped[0] != 1 || len(raw) != 2 || raw[0] != 3 {
		t.Fatalf("unexpected results: %v %v", enveloped, raw)
	}
}

func TestBenchmarksDecodeDynamicCategoriesAndIgnoreMetadata(t *testing.T) {
	payload := []byte(`{
		"aaData":{
			"intelligence":[{"permaslug":"acme/a","score":70}],
			"math":[{"permaslug":"acme/b","score":80}],
			"percentilesBySlug":{"acme/a":0.9}
		},
		"daData":{
			"models-website":[{"openrouter_id":"acme/a","display_name":"A","score":1400}],
			"agents-brand-new":[{"openrouter_id":"acme/b","display_name":"B","score":1500}]
		},
		"weightedInputPrices":{},
		"costPerRequest":{}
	}`)
	var benchmarks Benchmarks
	if err := json.Unmarshal(payload, &benchmarks); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(benchmarks.AllAACategories(), ","); got != "intelligence,math" {
		t.Fatalf("unexpected AA categories %q", got)
	}
	if _, exists := benchmarks.AA["percentilesBySlug"]; exists {
		t.Fatal("metadata was treated as a leaderboard")
	}
	if categories := benchmarks.DACategories(); len(categories) != 2 || categories[1].Key != "agents-brand-new" {
		t.Fatalf("unexpected Design Arena categories: %#v", categories)
	}
}

func TestBenchmarksRejectMalformedAndSemanticallyEmptyPayloads(t *testing.T) {
	var benchmarks Benchmarks
	if err := json.Unmarshal([]byte(`{"aaData":{"coding":[invalid]}}`), &benchmarks); err == nil {
		t.Fatal("malformed leaderboard was accepted")
	}

	client, _ := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":{"aaData":{"percentilesBySlug":{}},"daData":{}}}`))
	}))
	_, err := client.Benchmarks(true)
	var shapeErr UnexpectedShapeError
	if !errors.As(err, &shapeErr) {
		t.Fatalf("expected shape error, got %v", err)
	}
}

func TestForcedRefreshNeverFallsBackToStaleCache(t *testing.T) {
	var fail atomic.Bool
	client, server := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fail.Load() {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))

	first, err := client.cachedFetchURL("strict", server.URL, true, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	fail.Store(true)
	stale, err := client.cachedFetchURL("strict", server.URL, false, -time.Second)
	if err != nil || string(stale) != string(first) {
		t.Fatalf("normal read did not use stale fallback: %q, %v", stale, err)
	}
	if _, err := client.cachedFetchURL("strict", server.URL, true, time.Minute); err == nil {
		t.Fatal("forced refresh silently used stale cache")
	}
}

func TestEndpointsRefreshBypassesFreshCache(t *testing.T) {
	var calls atomic.Int32
	client, _ := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call := calls.Add(1)
		_, _ = w.Write([]byte(`{"data":{"id":"acme/model","name":"call-` + string(rune('0'+call)) + `"}}`))
	}))

	first, err := client.Endpoints("acme/model")
	if err != nil {
		t.Fatal(err)
	}
	cached, err := client.Endpoints("acme/model")
	if err != nil {
		t.Fatal(err)
	}
	refreshed, err := client.Endpoints("acme/model", true)
	if err != nil {
		t.Fatal(err)
	}
	if first.Name != "call-1" || cached.Name != "call-1" || refreshed.Name != "call-2" {
		t.Fatalf("unexpected endpoint cache behavior: %q %q %q", first.Name, cached.Name, refreshed.Name)
	}
}

func TestJSONKeepsExistingKeysWhileAddingLiveMetadata(t *testing.T) {
	providerJSON, err := json.Marshal(Provider{Name: "Acme", Slug: "acme", BYOKEnabled: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"name"`, `"slug"`, `"byokEnabled"`} {
		if !strings.Contains(string(providerJSON), key) {
			t.Fatalf("provider JSON missing %s: %s", key, providerJSON)
		}
	}

	benchmarkJSON, err := json.Marshal(Benchmarks{
		AA: map[string][]AAScore{"coding": {}},
		DA: map[string][]DARow{"models-website": {}},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"aaData"`, `"daData"`, `"weightedInputPrices"`, `"costPerRequest"`, `"fetched_at"`} {
		if !strings.Contains(string(benchmarkJSON), key) {
			t.Fatalf("benchmark JSON missing %s: %s", key, benchmarkJSON)
		}
	}
}
