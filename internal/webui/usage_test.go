package webui

import "testing"

func TestForwardFillPreservesLeadingNulls(t *testing.T) {
	f := func(v float64) *float64 { return &v }
	arr := []*float64{nil, nil, f(40), nil, nil, f(60), nil}
	forwardFill(arr)
	if arr[0] != nil || arr[1] != nil {
		t.Fatalf("leading nulls modified: %+v", arr)
	}
	for i, want := range []float64{40, 40, 60, 60} {
		got := arr[i+3]
		if got == nil || *got != want {
			t.Fatalf("bucket %d want %v, got %v", i+3, want, got)
		}
	}
}

func TestForwardFillGapsStopsCarry(t *testing.T) {
	f := func(v float64) *float64 { return &v }
	// Bucket 2 had a snapshot with no reading: it must stay null and must not
	// let 40 carry past it into bucket 3.
	arr := []*float64{f(40), nil, f(0), nil, f(55), nil}
	forwardFillGaps(arr, []bool{false, false, true, false, false, false})
	want := []*float64{f(40), f(40), nil, nil, f(55), f(55)}
	for i := range want {
		switch {
		case want[i] == nil && arr[i] != nil:
			t.Fatalf("bucket %d want null, got %v", i, *arr[i])
		case want[i] != nil && (arr[i] == nil || *arr[i] != *want[i]):
			t.Fatalf("bucket %d want %v, got %v", i, *want[i], arr[i])
		}
	}
}

func TestUnreportedOnlyForMissingZero(t *testing.T) {
	for _, tc := range []struct {
		pct      float64
		observed bool
		want     bool
	}{
		{0, false, true},   // null bucket stored as placeholder 0
		{0, true, false},   // genuine measured 0%
		{40, false, false}, // legacy row from before the observed columns
		{40, true, false},
	} {
		if got := unreported(tc.pct, tc.observed); got != tc.want {
			t.Errorf("unreported(%v, %v) = %v, want %v", tc.pct, tc.observed, got, tc.want)
		}
	}
}
