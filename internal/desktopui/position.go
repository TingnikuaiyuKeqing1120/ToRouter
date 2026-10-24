package desktopui

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
)

type petPosition struct {
	Left   int `json:"left"`
	Bottom int `json:"bottom"`
}

const petPositionFile = ".pet-position.json"

// Coordinates are physical desktop pixels; bottom is independent of the
// currently expanded dock height. Missing/invalid optional cache uses default.
func readPetPosition(dataDir string) (petPosition, bool) {
	if dataDir == "" {
		return petPosition{}, false
	}
	f, err := os.Open(filepath.Join(dataDir, petPositionFile))
	if err != nil {
		return petPosition{}, false
	}
	defer f.Close()
	var raw struct {
		Left   *int `json:"left"`
		Bottom *int `json:"bottom"`
	}
	dec := json.NewDecoder(io.LimitReader(f, 1024))
	dec.DisallowUnknownFields()
	if dec.Decode(&raw) != nil || raw.Left == nil || raw.Bottom == nil || dec.Decode(new(any)) != io.EOF {
		return petPosition{}, false
	}
	const limit = 1 << 30
	if *raw.Left < -limit || *raw.Left > limit || *raw.Bottom < -limit || *raw.Bottom > limit {
		return petPosition{}, false
	}
	return petPosition{*raw.Left, *raw.Bottom}, true
}

func writePetPosition(dataDir string, value petPosition) error {
	if dataDir == "" {
		return nil
	}
	f, err := os.CreateTemp(dataDir, ".pet-position-*")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if err := json.NewEncoder(f).Encode(value); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(name, filepath.Join(dataDir, petPositionFile))
}

func (a *petAnchor) position(current windowRect) petPosition {
	bottom := current.bottom
	if a.valid {
		bottom = a.bottom + current.top - a.lastTop
	}
	return petPosition{current.left, bottom}
}

type petPlacement interface {
	resize(int) error
	restore() error
	save() error
}
