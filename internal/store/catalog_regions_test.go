package store

import "testing"

func selectedPrefillTable(t *testing.T, price *Price) *PriceTable {
	t.Helper()
	if price == nil {
		t.Fatal("prefill produced no price")
	}
	if price.Selection.Mode == "manual" {
		return price.Selection.Table
	}
	ref := price.Selection.Ref
	if ref != nil {
		for _, item := range BuiltinCatalog {
			if item.Source != ref.Source || item.ID != ref.ID {
				continue
			}
			for _, offering := range item.Offerings {
				if offering.Key != ref.OfferingKey {
					continue
				}
				for _, quote := range offering.Quotes {
					if quote.Key == ref.QuoteKey {
						return &quote.Table
					}
				}
			}
		}
	}
	t.Fatal("prefill returned an unresolved catalog price", ref)
	return nil
}

func TestRegionalPrefillSelectsWholeQuote(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, mode := range []string{"catalog", "manual"} {
		for _, kind := range []string{"downstream", "upstream"} {
			for _, region := range []struct {
				key, currency string
				input         float64
			}{{"xiaomi/china/realtime", "CNY", 3}, {"xiaomi/overseas/realtime", "USD", .435}} {
				t.Run(mode+"/"+kind+"/"+region.currency, func(t *testing.T) {
					ref := &CatalogModelRef{Source: "builtin", ID: "mimo-v2.6-pro", OfferingKey: region.key}
					p, err := s.PreviewCatalogPrefill(PrefillRequest{Targets: []PrefillTarget{{Kind: kind, Model: "draft-model", Draft: &ModelConfig{}, Ref: ref}}, Groups: []string{"price"}, PriceMode: mode})
					if err != nil {
						t.Fatal(err)
					}
					patch := p.Patches[0]
					if patch.Price == nil || patch.CatalogRef == nil || *patch.CatalogRef != *ref {
						t.Fatalf("selected region did not fill price and association: price=%+v ref=%+v warnings=%v", patch.Price, patch.CatalogRef, patch.Warnings)
					}
					table := selectedPrefillTable(t, patch.Price)
					if table == nil || table.Currency != region.currency || table.Rates.Input == nil || *table.Rates.Input != region.input {
						t.Fatalf("selected wrong regional price: %+v", table)
					}
					if mode == "catalog" && (patch.Price.Selection.Ref == nil || patch.Price.Selection.Ref.OfferingKey != region.key) {
						t.Fatal("catalog reference lost region", patch.Price)
					}
					if mode == "manual" && (patch.Price.Selection.Table == nil || patch.Price.Selection.Table.Currency != region.currency) {
						t.Fatal("manual snapshot lost regional currency", patch.Price)
					}
				})
			}
		}
	}
	ref := &CatalogModelRef{Source: "builtin", ID: "mimo-v2.6-pro"}
	req := PrefillRequest{Targets: []PrefillTarget{{Kind: "downstream", Model: "draft-model", Draft: &ModelConfig{}, Ref: ref}}, Groups: []string{"price"}}
	p, err := s.PreviewCatalogPrefill(req)
	if err != nil || p.Patches[0].Price != nil {
		t.Fatal("unselected region must not pick the first quote", p, err)
	}
	ref.OfferingKey = "xiaomi/china/realtime"
	req.Targets[0].PriceRef = &CatalogPriceRef{Source: "builtin", ID: ref.ID, OfferingKey: "xiaomi/overseas/realtime", QuoteKey: "token"}
	p, err = s.PreviewCatalogPrefill(req)
	if err != nil || p.Patches[0].Price != nil {
		t.Fatal("stale explicit quote crossed selected region", p, err)
	}
	ref.OfferingKey = "removed-region"
	if _, err = s.PreviewCatalogPrefill(req); err == nil {
		t.Fatal("missing region silently accepted")
	}
}

func TestRegionalPrefillKeepsCacheTTLChoice(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ref := &CatalogModelRef{Source: "builtin", ID: "kimi-k3", OfferingKey: "moonshot/china/payg"}
	req := PrefillRequest{Targets: []PrefillTarget{{Kind: "downstream", Model: "draft", Draft: &ModelConfig{}, Ref: ref}}, Groups: []string{"price"}}
	p, err := s.PreviewCatalogPrefill(req)
	if err != nil || p.Patches[0].Price != nil {
		t.Fatal("multiple cache TTLs must remain a quote choice", p, err)
	}
	req.Targets[0].PriceRef = &CatalogPriceRef{Source: ref.Source, ID: ref.ID, OfferingKey: ref.OfferingKey, QuoteKey: "cache-1h"}
	p, err = s.PreviewCatalogPrefill(req)
	if err != nil {
		t.Fatal(err)
	}
	table := selectedPrefillTable(t, p.Patches[0].Price)
	if table == nil || table.Currency != "CNY" || *table.Rates.CacheWrite != 40 {
		t.Fatal("wrong TTL or region", table)
	}
}

func TestRegionalPrefillReusesSavedRefsInBatch(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	cn := CatalogModelRef{Source: "builtin", ID: "mimo-v2.6-pro", OfferingKey: "xiaomi/china/realtime"}
	us := CatalogModelRef{Source: "builtin", ID: "mimo-v2.6-flash", OfferingKey: "xiaomi/overseas/realtime"}
	c := Channel{Name: "regional", Enabled: true, Models: []string{cn.ID, us.ID}, APIKey: "test-key", Endpoints: []Endpoint{{Format: FormatOpenAIChatCompletions, BaseURL: "https://example.com"}}, ModelCatalogRefs: map[string]CatalogModelRef{cn.ID: cn, us.ID: us}}
	if err := s.SaveChannel(&c); err != nil {
		t.Fatal(err)
	}
	targets := []PrefillTarget{}
	for _, ref := range []CatalogModelRef{cn, us} {
		m := DownstreamModel{ID: "alias-" + ref.ID, Enabled: true, CatalogModelRef: &ref, Bindings: []Binding{{ChannelID: c.ID, Model: ref.ID, Enabled: true}}}
		if err := s.SaveDownstreamModel(m); err != nil {
			t.Fatal(err)
		}
		targets = append(targets, PrefillTarget{Kind: "downstream", Model: m.ID}, PrefillTarget{Kind: "upstream", Model: ref.ID, ChannelID: c.ID})
	}
	req := PrefillRequest{Targets: targets, Groups: []string{"price"}}
	p, err := s.PreviewCatalogPrefill(req)
	if err != nil {
		t.Fatal(err)
	}
	for i, patch := range p.Patches {
		want := cn.OfferingKey
		if i >= 2 {
			want = us.OfferingKey
		}
		if patch.Price == nil || patch.Price.Selection.Ref.OfferingKey != want || patch.CatalogRef.OfferingKey != want {
			t.Fatalf("batch lost saved region: price=%+v ref=%+v", patch.Price, patch.CatalogRef)
		}
	}
	if _, err = s.ApplyCatalogPrefill(req); err != nil {
		t.Fatal(err)
	}
	prices, err := s.GetPrices()
	if err != nil || len(prices) != 4 {
		t.Fatal("regional batch did not persist all prices", prices, err)
	}
}
