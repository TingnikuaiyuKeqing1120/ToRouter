package store

import (
	_ "embed"
	"encoding/json"
)

//go:embed builtin_models.json
var builtinModelsJSON []byte

// BuiltinCatalog is the offline first-party catalog. Updating it never changes
// saved model configuration; consumers explicitly choose a model or quote.
var BuiltinCatalog = loadBuiltinCatalog()

func loadBuiltinCatalog() []CatalogItem {
	var items []CatalogItem
	if err := json.Unmarshal(builtinModelsJSON, &items); err != nil {
		panic("invalid embedded model catalog: " + err.Error())
	}
	return items
}
