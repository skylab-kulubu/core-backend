package certificate

const (
	RuleNone  = "none"
	RuleOnce  = "once"
	RuleRatio = "ratio"
)

func Eligible(rule string, ratio float64, checkIns, scheduled int) bool {
	if scheduled <= 0 {
		return false
	}
	switch rule {
	case RuleOnce:
		return checkIns >= 1
	case RuleRatio:
		return float64(checkIns)/float64(scheduled) >= ratio
	default:
		return false
	}
}
