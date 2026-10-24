package desktopui

// All values use one coordinate space: physical desktop pixels on Windows,
// including negative coordinates on monitors left of/above the primary.
type windowRect struct{ left, top, right, bottom int }
type petAnchor struct {
	valid           bool
	bottom, lastTop int
}

func (a *petAnchor) resize(current, work windowRect, width, height int) windowRect {
	if !a.valid {
		a.bottom = current.bottom
		a.valid = true
	} else {
		// Incorporate dragging while expanded without treating our temporary
		// work-area clamp as a change to the preferred bottom anchor.
		a.bottom += current.top - a.lastTop
	}
	x := current.left
	if x+width > work.right {
		x = work.right - width
	}
	if x < work.left {
		x = work.left
	}
	y := a.bottom - height
	if y+height > work.bottom {
		y = work.bottom - height
	}
	if y < work.top {
		y = work.top
	}
	a.lastTop = y
	return windowRect{x, y, x + width, y + height}
}
