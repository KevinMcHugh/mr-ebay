package searches_test

import (
	"errors"
	"slices"
	"testing"

	"github.com/KevinMcHugh/mr-ebay/internal/app"
	"github.com/KevinMcHugh/mr-ebay/internal/server/searches"
)

func TestCleanNormalizes(t *testing.T) {
	f, err := searches.Clean(searches.Fields{
		Name:          "  rain   jacket ",
		Query:         "arcteryx  beta",
		Conditions:    []string{"used", "NEW", "used", " "},
		CategoryIDs:   []string{"57988"},
		MaxPriceCents: 20000,
	})
	if err != nil {
		t.Fatal(err)
	}
	if f.Name != "rain jacket" || f.Query != "arcteryx beta" || f.Currency != "USD" || f.Marketplace != "EBAY_US" {
		t.Fatalf("cleaned %+v", f)
	}
	if !slices.Equal(f.Conditions, []string{"NEW", "USED"}) || f.BuyingOptions == nil {
		t.Fatalf("sets %+v", f)
	}
}

func TestCleanRefuses(t *testing.T) {
	ok := searches.Fields{Name: "n", Query: "q"}
	for name, mutate := range map[string]func(*searches.Fields){
		"no name":         func(f *searches.Fields) { f.Name = " " },
		"no query":        func(f *searches.Fields) { f.Query = "" },
		"bad condition":   func(f *searches.Fields) { f.Conditions = []string{"MINT"} },
		"bad option":      func(f *searches.Fields) { f.BuyingOptions = []string{"BARTER"} },
		"bad category":    func(f *searches.Fields) { f.CategoryIDs = []string{"jackets"} },
		"negative price":  func(f *searches.Fields) { f.MinPriceCents = -1 },
		"min above max":   func(f *searches.Fields) { f.MinPriceCents, f.MaxPriceCents = 500, 100 },
		"bad currency":    func(f *searches.Fields) { f.Currency = "dollars" },
		"bad marketplace": func(f *searches.Fields) { f.Marketplace = "AMAZON" },
	} {
		f := ok
		mutate(&f)
		if _, err := searches.Clean(f); !errors.Is(err, app.ErrBadInput) {
			t.Errorf("%s: err %v, want ErrBadInput", name, err)
		}
	}
	// A minimum with no maximum is fine.
	if _, err := searches.Clean(searches.Fields{Name: "n", Query: "q", MinPriceCents: 500}); err != nil {
		t.Errorf("min only: %v", err)
	}
}
