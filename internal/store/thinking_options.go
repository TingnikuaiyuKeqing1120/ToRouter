package store

import "slices"

// ThinkingOptions returns the configured choice range separately from native
// request metadata. A declared empty range remains distinct from an unknown range.
func ThinkingOptions(c ModelConfig) ([]string, bool) {
	if c.ThinkingEfforts != nil {
		return append([]string{}, (*c.ThinkingEfforts)...), true
	}
	n := c.NativeThinking
	if n == nil || n.Supported == nil {
		return nil, false
	}
	options := []string{}
	if !*n.Supported {
		return options, true
	}
	// Support alone does not establish a choice range. A partial effort enum
	// cannot be completed from a default effort or from another mode's choices.
	if len(n.Modes) == 0 {
		return nil, false
	}
	known := false
	for _, m := range n.Modes {
		if m.Control == "effort" && m.Efforts == nil {
			return nil, false
		}
		if m.Efforts != nil || m.Control == "enabled" || m.Control == "adaptive" {
			known = true
		}
	}
	if !known {
		return nil, false
	}
	add := func(value string) {
		if !slices.Contains(options, value) {
			options = append(options, value)
		}
	}
	if n.Mandatory != nil && *n.Mandatory {
		add("enable")
	}
	for _, m := range n.Modes {
		for _, effort := range m.Efforts {
			add(effort)
		}
		switch m.Control {
		case "enabled":
			add("enable")
			if n.Mandatory == nil || !*n.Mandatory {
				add("disable")
			}
		case "adaptive":
			add("adaptive")
			add("enable")
			if n.Mandatory != nil && !*n.Mandatory {
				add("disable")
			}
		case "effort":
			if m.DefaultEffort != nil && *m.DefaultEffort != "none" {
				add("enable")
			}
			if n.Mandatory != nil && !*n.Mandatory && slices.Contains(m.Efforts, "none") {
				add("disable")
			}
		}
	}
	return options, true
}
