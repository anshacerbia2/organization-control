package db

import (
	"errors"
	"fmt"

	"github.com/anshacerbia2/foundation-platform/id"
)

// The page size a list returns when none is asked for, and the most it returns (STD-GLB-001 1.3.0
// §Pagination). The same numbers identity-control serves, so an estate client pages both alike.
const (
	DefaultListLimit = 50
	MaxListLimit     = 100
)

// ErrListLimit reports a page size outside 1 to MaxListLimit. Every domain package wraps it in its
// own ErrInvalid, so the HTTP surface answers 400 rather than coercing: a silent reduction would hide
// the bound from the client (STD-GLB-001 1.3.0 §Pagination).
var ErrListLimit = errors.New("db: the page size is out of range")

// ListLimit resolves a requested page size. Zero asks for the default; anything below one or above
// MaxListLimit is refused.
//
// It lives here because every list in this repository is a keyset over a primary key, read in a
// transaction this package opens, and the five that exist must not disagree about the bound.
func ListLimit(requested int) (int, error) {
	switch {
	case requested == 0:
		return DefaultListLimit, nil
	case requested < 0 || requested > MaxListLimit:
		return 0, fmt.Errorf("%w: limit must be between 1 and %d", ErrListLimit, MaxListLimit)
	}
	return requested, nil
}

// Keyset is the cursor argument of a keyset statement: the nil identifier as SQL NULL, so
// `($n::uuid IS NULL OR key > $n::uuid)` starts at the first row, and the identifier's text otherwise.
func Keyset(after id.UUID) any {
	if after.IsNil() {
		return nil
	}
	return after.String()
}
