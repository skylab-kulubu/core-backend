package ticket

// NormalizePhone puts a phone number in the Walk-in's one form
// (normalizePhone): +90 and ten digits for a Turkish number, + and 8 to 15
// digits for another country's. false is not a phone number. A form
// mapping's phone answer takes it too (docs/form-mappings.md).
func NormalizePhone(raw string) (string, bool) { return normalizePhone(raw) }
