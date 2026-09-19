package lifecycle

import "errors"

var ErrInvalidVisibility = errors.New("lifecycle: invalid visibility")

// Visibility controls whether repository queries return current or inactive
// durable records. Normal product reads must use CurrentOnly.
type Visibility uint8

const (
	CurrentOnly Visibility = iota
	InactiveOnly
	All
)

// ParseVisibility converts the public lifecycle query value into a repository
// visibility. An empty value preserves the normal current-record view.
func ParseVisibility(raw string) (Visibility, error) {
	switch raw {
	case "", "current":
		return CurrentOnly, nil
	case "inactive":
		return InactiveOnly, nil
	case "all":
		return All, nil
	default:
		return CurrentOnly, ErrInvalidVisibility
	}
}

// Matches reports whether a record with the supplied lifecycle state belongs
// in this view. Unknown values deliberately fail closed to the normal current
// view so a malformed management request cannot expose inactive records.
func (v Visibility) Matches(inactive bool) bool {
	switch v {
	case InactiveOnly:
		return inactive
	case All:
		return true
	default:
		return !inactive
	}
}

// SQLCondition returns the SQL predicate for a trusted lifecycle column name.
// Callers must supply a static column identifier, never request input.
func (v Visibility) SQLCondition(column string) string {
	switch v {
	case InactiveOnly:
		return column + " IS NOT NULL"
	case All:
		return ""
	default:
		return column + " IS NULL"
	}
}
