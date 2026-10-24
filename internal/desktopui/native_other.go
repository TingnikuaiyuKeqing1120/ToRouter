//go:build !windows

package desktopui

import "errors"

func focusPetWindow() {}
func makePetToolWindow() error {
	return errors.New("native tool windows are only supported on Windows")
}
func acquirePetLaunchGate(string) (func(), error) { return func() {}, nil }
func newPetResizer() func(int) error {
	return func(int) error { return errors.New("native companion sizing is only supported on Windows") }
}

type unsupportedPetPlacement struct{}

func newPetPlacement(string) petPlacement { return unsupportedPetPlacement{} }
func (unsupportedPetPlacement) resize(int) error {
	return errors.New("native companion sizing is only supported on Windows")
}
func (unsupportedPetPlacement) restore() error { return nil }
func (unsupportedPetPlacement) save() error    { return nil }
