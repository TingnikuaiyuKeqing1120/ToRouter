package store

type ModelSpec struct {
	ChannelID       int64            `json:"channel_id"`
	ChannelName     string           `json:"channel_name"`
	Model           string           `json:"model"`
	Specs           ModelConfig      `json:"specs"`
	CatalogModelRef *CatalogModelRef `json:"catalog_model_ref,omitempty"`
}

func (s *Store) ListModelSpecs() ([]ModelSpec, error) {
	channels, err := s.ListChannels()
	if err != nil {
		return nil, err
	}
	result := []ModelSpec{}
	for _, c := range channels {
		seen := map[string]bool{}
		for _, model := range append(append([]string{}, c.Models...), c.AvailableModels...) {
			if seen[model] {
				continue
			}
			seen[model] = true
			row := ModelSpec{ChannelID: c.ID, ChannelName: c.Name, Model: model, Specs: c.ModelConfigs[model]}
			if ref, ok := c.ModelCatalogRefs[model]; ok {
				row.CatalogModelRef = &ref
			}
			result = append(result, row)
		}
	}
	return result, nil
}

// SaveModelSpecs shares the connection lock but changes only this model's specifications.
func (s *Store) SaveModelSpecs(row ModelSpec) error {
	s.channelSaveMu.Lock()
	defer s.channelSaveMu.Unlock()
	c, err := s.channel(row.ChannelID)
	if err != nil {
		return err
	}
	if !containsString(c.Models, row.Model) && !containsString(c.AvailableModels, row.Model) {
		return NewConfigurationError("upstream model is not configured or discovered")
	}
	if err = ValidateModelConfig(row.Specs); err != nil {
		return err
	}
	if row.CatalogModelRef != nil {
		if err = ValidateCatalogModelRef(*row.CatalogModelRef); err != nil {
			return err
		}
	}
	if c.ModelConfigs == nil {
		c.ModelConfigs = map[string]ModelConfig{}
	}
	c.ModelConfigs[row.Model] = row.Specs
	if c.ModelCatalogRefs == nil {
		c.ModelCatalogRefs = map[string]CatalogModelRef{}
	}
	if row.CatalogModelRef == nil {
		delete(c.ModelCatalogRefs, row.Model)
	} else {
		c.ModelCatalogRefs[row.Model] = *row.CatalogModelRef
	}
	return s.writeChannelInPlace(&c)
}
