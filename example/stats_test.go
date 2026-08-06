package example

import "testing"

func TestSum(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		values []int
		want   int
	}{
		"empty":     {want: 0},
		"one value": {values: []int{7}, want: 7},
		"several":   {values: []int{1, 2, 3}, want: 6},
		"negatives": {values: []int{5, -2, -1}, want: 2},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			if got := Sum(tt.values); got != tt.want {
				t.Errorf("Sum(%v) = %d, want %d", tt.values, got, tt.want)
			}
		})
	}
}

func TestMean(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		values []int
		want   int
	}{
		"no data truncates to zero": {want: 0},
		"exact":                     {values: []int{2, 4, 6}, want: 4},
		"truncated towards zero":    {values: []int{1, 2, 3, 4}, want: 2},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			if got := Mean(tt.values); got != tt.want {
				t.Errorf("Mean(%v) = %d, want %d", tt.values, got, tt.want)
			}
		})
	}
}

func TestCountAbove(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		values    []int
		threshold int
		want      int
	}{
		"empty":                      {threshold: 0, want: 0},
		"some above":                 {values: []int{1, 5, 3, 9}, threshold: 3, want: 2},
		"the threshold is exclusive": {values: []int{3, 3, 3}, threshold: 3, want: 0},
		"all above":                  {values: []int{4, 5, 6}, threshold: 3, want: 3},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			if got := CountAbove(tt.values, tt.threshold); got != tt.want {
				t.Errorf("CountAbove(%v, %d) = %d, want %d", tt.values, tt.threshold, got, tt.want)
			}
		})
	}
}

func TestBounds(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		values            []int
		wantLow, wantHigh int
		wantOK            bool
	}{
		"empty":     {},
		"one value": {values: []int{4}, wantLow: 4, wantHigh: 4, wantOK: true},
		"rising":    {values: []int{5, 9, 7}, wantLow: 5, wantHigh: 9, wantOK: true},
		"all equal": {values: []int{2, 2, 2}, wantLow: 2, wantHigh: 2, wantOK: true},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			low, high, ok := Bounds(tt.values)
			if low != tt.wantLow || high != tt.wantHigh || ok != tt.wantOK {
				t.Errorf("Bounds(%v) = %d, %d, %v; want %d, %d, %v",
					tt.values, low, high, ok, tt.wantLow, tt.wantHigh, tt.wantOK)
			}
		})
	}
}

func TestTrend(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		values []int
		want   string
	}{
		"no values":  {want: "flat"},
		"one value":  {values: []int{3}, want: "flat"},
		"rising":     {values: []int{1, 2, 3}, want: "rising"},
		"unchanging": {values: []int{2, 2, 2}, want: "flat"},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			if got := Trend(tt.values); got != tt.want {
				t.Errorf("Trend(%v) = %q, want %q", tt.values, got, tt.want)
			}
		})
	}
}
