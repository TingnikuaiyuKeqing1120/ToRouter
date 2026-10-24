package store

import (
	"reflect"
	"testing"
)

func TestExchangeSourcesPersistOrderAndDisabledSelection(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	ids, err := s.GetExchangeSources()
	if err != nil || !reflect.DeepEqual(ids, []string{"ecb", "frankfurter", "exchangerate_api"}) {
		t.Fatal(ids, err)
	}
	desired := []string{"exchangerate_api", "ecb"}
	if err := s.SaveExchangeSources(desired); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ids, err = s.GetExchangeSources()
	if err != nil || !reflect.DeepEqual(ids, desired) {
		t.Fatal(ids, err)
	}
	for _, invalid := range [][]string{nil, {"ecb", "ecb"}, {"http://arbitrary.test"}} {
		if err := s.SaveExchangeSources(invalid); err == nil {
			t.Fatalf("accepted %v", invalid)
		}
	}
	ids, _ = s.GetExchangeSources()
	if !reflect.DeepEqual(ids, desired) {
		t.Fatal("invalid config changed sources")
	}
	if err := s.SaveExchangeSources([]string{}); err != nil {
		t.Fatal(err)
	}
	ids, err = s.GetExchangeSources()
	if err != nil || ids == nil || len(ids) != 0 {
		t.Fatal(ids, err)
	}
}
