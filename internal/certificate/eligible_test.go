package certificate

import "testing"

func TestEligible_NoneNever(t *testing.T) {
	t.Parallel()
	if Eligible(RuleNone, 0, 8, 8) {
		t.Fatal("none must not issue")
	}
}

func TestEligible_ZeroSessionsNever(t *testing.T) {
	t.Parallel()
	if Eligible(RuleOnce, 0, 0, 0) {
		t.Fatal("once with zero sessions")
	}
	if Eligible(RuleRatio, 0.75, 0, 0) {
		t.Fatal("ratio with zero sessions")
	}
}

func TestEligible_OnceNeedsOneOturum(t *testing.T) {
	t.Parallel()
	if Eligible(RuleOnce, 0, 0, 8) {
		t.Fatal("zero check-ins")
	}
	if !Eligible(RuleOnce, 0, 1, 8) {
		t.Fatal("one check-in should issue")
	}
}

func TestEligible_ARTLABRatio075(t *testing.T) {
	t.Parallel()
	if Eligible(RuleRatio, 0.75, 5, 8) {
		t.Fatal("5 of 8 must not issue")
	}
	if !Eligible(RuleRatio, 0.75, 6, 8) {
		t.Fatal("6 of 8 must issue")
	}
}

func TestEligible_EmptyRuleIsNone(t *testing.T) {
	t.Parallel()
	if Eligible("", 0.75, 8, 8) {
		t.Fatal("unset rule is none")
	}
}
