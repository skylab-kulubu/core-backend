package certificate

import "testing"

// legacy is the rule as the decision of before the count rule took it: a
// kind and a ratio (the cases below are those of before, on the new
// signature: every live Session counted, none required).
func legacy(kind string, ratio float64) Rule {
	return Rule{Kind: kind, Ratio: &ratio}
}

func TestEligible_NoneNever(t *testing.T) {
	t.Parallel()
	if Eligible(legacy(RuleNone, 0), 8, 8, 0) {
		t.Fatal("none must not issue")
	}
}

func TestEligible_ZeroSessionsNever(t *testing.T) {
	t.Parallel()
	if Eligible(legacy(RuleOnce, 0), 0, 0, 0) {
		t.Fatal("once with zero sessions")
	}
	if Eligible(legacy(RuleRatio, 0.75), 0, 0, 0) {
		t.Fatal("ratio with zero sessions")
	}
}

func TestEligible_OnceNeedsOneOturum(t *testing.T) {
	t.Parallel()
	if Eligible(legacy(RuleOnce, 0), 0, 8, 0) {
		t.Fatal("zero check-ins")
	}
	if !Eligible(legacy(RuleOnce, 0), 1, 8, 0) {
		t.Fatal("one check-in should issue")
	}
}

func TestEligible_ARTLABRatio075(t *testing.T) {
	t.Parallel()
	if Eligible(legacy(RuleRatio, 0.75), 5, 8, 0) {
		t.Fatal("5 of 8 must not issue")
	}
	if !Eligible(legacy(RuleRatio, 0.75), 6, 8, 0) {
		t.Fatal("6 of 8 must issue")
	}
}

func TestEligible_EmptyRuleIsNone(t *testing.T) {
	t.Parallel()
	if Eligible(legacy("", 0.75), 8, 8, 0) {
		t.Fatal("unset rule is none")
	}
}

func intp(v int) *int { return &v }

func TestEffectiveMin(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		minCount *int
		counted  int
		want     int
	}{
		{"no sessions", nil, 0, 0},
		{"no sessions with M", intp(3), 0, 0},
		{"default N=1", nil, 1, 1},
		{"default N=3", nil, 3, 3},
		{"default N=4", nil, 4, 3},
		{"default N=8", nil, 8, 6},
		{"default N=10", nil, 10, 8},
		{"M above N", intp(6), 4, 4},
		{"M below N", intp(2), 8, 2},
		{"M equal N", intp(8), 8, 8},
	}
	for _, tc := range cases {
		if got := EffectiveMin(tc.minCount, tc.counted); got != tc.want {
			t.Errorf("%s: EffectiveMin(%v, %d) = %d, want %d", tc.name, tc.minCount, tc.counted, got, tc.want)
		}
	}
}

func TestEligibleTable(t *testing.T) {
	t.Parallel()
	count := func(m *int) Rule { return Rule{Kind: RuleCount, MinCount: m} }
	cases := []struct {
		name                               string
		rule                               Rule
		attended, counted, missingRequired int
		want                               bool
	}{
		{"none, all attended", Rule{Kind: RuleNone}, 8, 8, 0, false},
		{"none with a threshold", Rule{Kind: RuleNone, MinCount: intp(1)}, 8, 8, 0, false},
		{"empty kind", Rule{}, 8, 8, 0, false},
		{"unknown kind", Rule{Kind: "most"}, 8, 8, 0, false},
		{"once, N=0", Rule{Kind: RuleOnce}, 1, 0, 0, false},
		{"count, N=0", count(nil), 0, 0, 0, false},
		{"count M=1, N=0", count(intp(1)), 3, 0, 0, false},
		{"ratio, N=0", legacy(RuleRatio, 0), 0, 0, 0, false},
		{"once, one check-in", Rule{Kind: RuleOnce}, 1, 8, 0, true},
		{"once, none", Rule{Kind: RuleOnce}, 0, 8, 0, false},
		{"once, one check-in, a required missing", Rule{Kind: RuleOnce}, 1, 8, 1, false},
		{"count default N=8, at 6", count(nil), 6, 8, 0, true},
		{"count default N=8, at 5", count(nil), 5, 8, 0, false},
		{"count default N=4, at 3", count(nil), 3, 4, 0, true},
		{"count default N=4, at 2", count(nil), 2, 4, 0, false},
		{"count M=6 N=8, at 6", count(intp(6)), 6, 8, 0, true},
		{"count M=6 N=8, at 5", count(intp(6)), 5, 8, 0, false},
		{"count M=6 N=4, at 4", count(intp(6)), 4, 4, 0, true},
		{"count M=6 N=4, at 3", count(intp(6)), 3, 4, 0, false},
		{"count at threshold, a required missing", count(intp(6)), 6, 8, 1, false},
		{"count all attended, a required missing", count(nil), 8, 8, 2, false},
		{"ratio 0.75, 6 of 8", legacy(RuleRatio, 0.75), 6, 8, 0, true},
		{"ratio 0.75, 5 of 8", legacy(RuleRatio, 0.75), 5, 8, 0, false},
		{"ratio 0.67, 2 of 3", legacy(RuleRatio, 0.67), 2, 3, 0, false},
		{"ratio 0.66, 2 of 3", legacy(RuleRatio, 0.66), 2, 3, 0, true},
		{"ratio nil is 0", Rule{Kind: RuleRatio}, 0, 3, 0, true},
		{"ratio, a required missing", legacy(RuleRatio, 0.5), 8, 8, 1, false},
	}
	for _, tc := range cases {
		if got := Eligible(tc.rule, tc.attended, tc.counted, tc.missingRequired); got != tc.want {
			t.Errorf("%s: Eligible = %v, want %v", tc.name, got, tc.want)
		}
	}
}
