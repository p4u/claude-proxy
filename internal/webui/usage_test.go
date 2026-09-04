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
