package dacost

import (
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/semistrict/dago/damessage"
)

func TestCacheTokenCountsSupportsBothInputConventionsAndClamps(t *testing.T) {
	read, writes := CacheTokenCounts(damessage.Usage{
		InputTokens: 20, OutputTokens: 50, TotalTokens: 150,
		InputDetails: map[string]int{
			"cache_read": 80, "cache_creation": 999,
			"ephemeral_5m_input_tokens": 70, "ephemeral_1h_input_tokens": 70,
		},
	})
	if read != 80 || writes != (CacheWrites{FiveMinute: 20}) {
		t.Fatalf("CacheTokenCounts() = %d, %#v", read, writes)
	}
	read, writes = CacheTokenCounts(damessage.Usage{
		InputTokens: 100, InputDetails: map[string]int{"cache_read": 30, "cache_write": 90},
	})
	if read != 30 || writes.Generic != 70 {
		t.Fatalf("CacheTokenCounts() = %d, %#v", read, writes)
	}
}

func TestPricerSeparatesCacheBucketsWithoutDoubleCounting(t *testing.T) {
	rates := Rates{
		InputMTok: new(1.0), OutputMTok: new(3.0), CacheReadMTok: new(0.1), CacheWriteMTok: new(2.0),
	}
	pricer := testPricer(map[string]Rates{"m": rates})
	cost, ok := pricer.Estimate("provider", "m", damessage.Usage{
		InputTokens: 20, OutputTokens: 10, TotalTokens: 110,
		InputDetails: map[string]int{"cache_read": 80, "cache_write": 20},
	})
	want := (80*0.1 + 20*2.0 + 10*3.0) / 1_000_000
	if !ok || math.Abs(cost-want) > 1e-12 {
		t.Fatalf("Estimate() = %.12f, %v; want %.12f", cost, ok, want)
	}
}

func TestPricerPrimaryThenLocalThenBundledAndAliases(t *testing.T) {
	primary := catalogWithModels("azure", map[string]float64{"primary": 1, "same": 1})
	bundled := catalogWithModels("azure", map[string]float64{"bundled": 4, "same": 4})
	local := catalogWithModels("azure", map[string]float64{"local": 9, "same": 9, "free": 0})
	pricer := NewPricer(primary, bundled, local, PricerOptions{})
	usage := damessage.Usage{InputTokens: 1_000_000}
	for _, test := range []struct {
		model string
		want  float64
	}{{"primary", 1}, {"same", 1}, {"local", 9}, {"bundled", 4}, {"free", 0}} {
		got, ok := pricer.Estimate("azure_openai", test.model, usage)
		if !ok || got != test.want {
			t.Fatalf("Estimate(%q) = %v, %v; want %v", test.model, got, ok, test.want)
		}
	}
	if _, ok := pricer.Estimate("ollama", "local", usage); ok {
		t.Fatal("local-only provider was priced")
	}
}

func TestLoadCatalogUsesBoundedProviderArraySchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "prices.json")
	document := `[{"id":"house","name":"House","api_pattern":"gateway\\.example","models":[{"id":"m","match":{"or":[{"equals":"m"},{"starts_with":"m-"}]},"prices":{"input_mtok":2.5,"output_mtok":10}}]}]`
	if err := os.WriteFile(path, []byte(document), 0o600); err != nil {
		t.Fatal(err)
	}
	catalog, err := LoadCatalog(path, CatalogOptions{})
	if err != nil {
		t.Fatal(err)
	}
	pricer := NewPricer(nil, nil, catalog, PricerOptions{})
	if cost, ok := pricer.Estimate("house", "m-v2", damessage.Usage{InputTokens: 1_000_000, OutputTokens: 1_000_000}); !ok || cost != 12.5 {
		t.Fatalf("Estimate() = %v, %v", cost, ok)
	}
	missing, err := LoadCatalog(filepath.Join(t.TempDir(), "missing.json"), CatalogOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := NewPricer(nil, nil, missing, PricerOptions{}).Estimate("house", "m", damessage.Usage{InputTokens: 1}); ok {
		t.Fatal("missing override file produced a price")
	}
}

func TestCatalogRejectsAdversarialInputs(t *testing.T) {
	cases := []string{
		`{"providers":[]}`,
		`[{"id":"p","id":"q","name":"P","api_pattern":"x","models":[]}]`,
		`[{"id":"p","name":"P","api_pattern":"[","models":[]}]`,
		`[{"id":"p","name":"P","api_pattern":"x","models":[{"id":"m","match":{"equals":"m","contains":"m"},"prices":{"input_mtok":1}}]}]`,
		`[{"id":"p","name":"P","api_pattern":"x","models":[{"id":"m","match":{"equals":"m"},"prices":{"input_mtok":-1}}]}]`,
	}
	for _, document := range cases {
		if _, err := DecodeCatalog(strings.NewReader(document), CatalogOptions{}); !errors.Is(err, ErrInvalidCatalog) {
			t.Fatalf("DecodeCatalog(%s) error = %v", document, err)
		}
	}
	oversized := `[{"id":"p","name":"P","api_pattern":"x","models":[]}]`
	if _, err := DecodeCatalog(strings.NewReader(oversized), CatalogOptions{MaxBytes: 8}); !errors.Is(err, ErrLimitExceeded) {
		t.Fatalf("oversized DecodeCatalog() error = %v", err)
	}
	path := filepath.Join(t.TempDir(), "catalog")
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadCatalog(path, CatalogOptions{}); !errors.Is(err, ErrInvalidCatalog) {
		t.Fatalf("directory LoadCatalog() error = %v", err)
	}
	emptyPath := filepath.Join(t.TempDir(), "empty.json")
	if err := os.WriteFile(emptyPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadCatalog(emptyPath, CatalogOptions{}); err != nil {
		t.Fatalf("empty LoadCatalog() error = %v", err)
	}
}

func TestPricerRejectsTotalOnlyAndNonfiniteCosts(t *testing.T) {
	pricer := testPricer(map[string]Rates{"m": {InputMTok: new(float64(maxRateUSDPerMillion))}})
	if _, ok := pricer.Estimate("provider", "m", damessage.Usage{TotalTokens: 100}); ok {
		t.Fatal("total-only usage was priced")
	}
	if _, ok := pricer.Estimate("provider", "m", damessage.Usage{InputTokens: math.MaxInt}); ok {
		t.Fatal("overflowed cost was accepted")
	}
}

func TestCatalogIsImmutableAndCrossProviderSweepIsOptIn(t *testing.T) {
	rate := 2.0
	providers := []ProviderPrices{{
		ID: "one", Name: "One", APIPattern: "one", Models: []ModelPrice{{
			ID: "m", Match: ModelMatch{Equals: "m"}, Prices: Rates{InputMTok: &rate},
		}},
	}}
	catalog := NewCatalog(providers, CatalogOptions{})
	rate = 99
	providers[0].Models[0].Match.Equals = "changed"
	usage := damessage.Usage{InputTokens: 1_000_000}
	strict := NewPricer(nil, nil, catalog, PricerOptions{})
	if _, ok := strict.Estimate("other", "m", usage); ok {
		t.Fatal("strict pricer crossed provider boundary")
	}
	permissive := NewPricer(nil, nil, catalog, PricerOptions{AllowCrossProviderMatch: true})
	if cost, ok := permissive.Estimate("other", "m", usage); !ok || cost != 2 {
		t.Fatalf("immutable permissive Estimate() = %v, %v", cost, ok)
	}
}

func TestBundledCatalogProvidesPinnedStopgapAndIsSafeToReuse(t *testing.T) {
	first := BundledCatalog()
	second := BundledCatalog()
	if first != second {
		t.Fatal("bundled catalog was rebuilt")
	}
	pricer := NewPricer(nil, first, nil, PricerOptions{})
	cost, ok := pricer.Estimate("baseten", "deepseek-ai/DeepSeek-V4-Flash-0731", damessage.Usage{
		InputTokens: 1_000_000, OutputTokens: 1_000_000,
	})
	if !ok || math.Abs(cost-0.39) > 1e-12 {
		t.Fatalf("cost = %v, ok = %t", cost, ok)
	}
}

func catalogWithModels(provider string, models map[string]float64) *Catalog {
	entries := make([]ModelPrice, 0, len(models))
	for model, rate := range models {
		entries = append(entries, ModelPrice{ID: model, Match: ModelMatch{Equals: model}, Prices: Rates{InputMTok: new(rate)}})
	}
	return NewCatalog([]ProviderPrices{{ID: provider, Name: provider, APIPattern: "example", Models: entries}}, CatalogOptions{})
}
