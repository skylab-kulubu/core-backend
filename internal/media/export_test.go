package media

// Unexported functions the external tests reach.
var (
	RebuildICCTags    = rebuildICCTags
	GuardICC          = guardICC
	CoverColorsInSlot = coverColorsInSlot
)
