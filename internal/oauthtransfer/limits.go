// Package oauthtransfer handles explicit native credential import and export.
package oauthtransfer

import "errors"

const MaxFileBytes = 4 << 20
const MaxTotalBytes = 8 << 20

var (
	ErrFile     = errors.New("credential file is invalid")
	ErrLimit    = errors.New("credential transfer exceeds size or count limit")
	ErrExpired  = errors.New("credential transfer preview expired or inaccessible")
	ErrConflict = errors.New("credential transfer target changed")
	ErrInvalid  = errors.New("invalid credential transfer selection")
)
