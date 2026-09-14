package pricing

import (
	"fmt"
	"testing"
)

// TestRoundTo_MatchesFmtFormatting is the property RoundTo exists for: its
// result must be exactly what fmt's %.*f verb would print for the same
// value/precision, so a CostComponent's AmountUSD and its Detail string
// (formatted with the same verb) can never disagree.
func TestRoundTo_MatchesFmtFormatting(t *testing.T) {
	cases := []struct {
		v        float64
		decimals int
	}{
		{135.89000000000001, 3},
		{1296.3300000000002, 3},
		{11.875, 3},
		{35.625, 3},
		{3.879089433672162, 2},
		{0.47869225826604483, 2},
		{0, 3},
		{25, 3},
	}
	for _, c := range cases {
		got := RoundTo(c.v, c.decimals)
		want := fmt.Sprintf("%.*f", c.decimals, c.v)
		gotStr := fmt.Sprintf("%.*f", c.decimals, got)
		if gotStr != want {
			t.Errorf("RoundTo(%v, %d) = %v (formats as %q), want it to format as %q", c.v, c.decimals, got, gotStr, want)
		}
	}
}

// TestRoundTo_FixesKnownFloatNoise covers the exact values reported as bugs:
// float64 sums/divisions carrying noise well past MoneyDecimals precision.
func TestRoundTo_FixesKnownFloatNoise(t *testing.T) {
	if got := RoundTo(135.89000000000001, MoneyDecimals); got != 135.89 {
		t.Errorf("RoundTo(135.89000000000001, MoneyDecimals) = %v, want 135.89", got)
	}
	if got := RoundTo(3.879089433672162, 2); got != 3.88 {
		t.Errorf("RoundTo(3.879089433672162, 2) = %v, want 3.88", got)
	}
}

// TestRoundTo_PreservesRealSubCentPrecision guards against MoneyDecimals=3
// being mistaken for a coarser rounding: 11.875 already carries real
// information at the third decimal place and must survive unchanged.
func TestRoundTo_PreservesRealSubCentPrecision(t *testing.T) {
	if got := RoundTo(11.875, MoneyDecimals); got != 11.875 {
		t.Errorf("RoundTo(11.875, MoneyDecimals) = %v, want 11.875 (unchanged)", got)
	}
	if got := RoundTo(35.625, MoneyDecimals); got != 35.625 {
		t.Errorf("RoundTo(35.625, MoneyDecimals) = %v, want 35.625 (unchanged)", got)
	}
}

// TestSumComponents_MatchesRoundedComponentSum covers the invariant a
// CostBreakdown.TotalUSD must hold: it always equals the sum of exactly
// what's itemized in Components, never a separately-computed, possibly
// drifting total.
func TestSumComponents_MatchesRoundedComponentSum(t *testing.T) {
	components := []CostComponent{
		{Name: "a", AmountUSD: 11.875},
		{Name: "b", AmountUSD: 35.625},
		{Name: "c", AmountUSD: 0.1},
		{Name: "d", AmountUSD: 0.2},
	}
	got := SumComponents(components)
	want := RoundTo(11.875+35.625+0.1+0.2, MoneyDecimals)
	if got != want {
		t.Errorf("SumComponents = %v, want %v", got, want)
	}
}
