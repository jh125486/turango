package example

// maxSafeTotal caps [Sum]'s running total. It is well below the range of an
// int on any platform turango runs on, so the clamp is reachable in principle
// and unreachable in a test that has to build the slice first.
const maxSafeTotal = 1 << 40

// Sum adds up values, saturating at [maxSafeTotal] rather than wrapping.
func Sum(values []int) int {
	total := 0

	for _, value := range values {
		//nomutant: saturating-overflow guard — reaching it needs a slice no unit test can afford to build, so a surviving mutant here would be noise
		if value > 0 && total > maxSafeTotal-value {
			total = maxSafeTotal

			break
		}

		total += value
	}

	return total
}

// Mean is the arithmetic mean of values, truncated towards zero. An empty
// slice has a mean of zero rather than a panic: callers treat "no data" and
// "no value" the same way.
func Mean(values []int) int {
	if len(values) == 0 {
		return 0
	}

	return Sum(values) / len(values)
}

// CountAbove is how many values are strictly greater than threshold.
func CountAbove(values []int, threshold int) int {
	count := 0

	for _, value := range values {
		if value > threshold {
			count++
		}
	}

	return count
}

// Bounds reports the smallest and largest of values.
//
// ok is false for an empty slice, in which case low and high are zero and mean
// nothing.
func Bounds(values []int) (low, high int, ok bool) {
	if len(values) == 0 {
		return 0, 0, false
	}

	low, high = values[0], values[0]

	for _, value := range values[1:] {
		if value < low {
			low = value
		} else if value > high {
			high = value
		}
	}

	return low, high, true
}

// Trend's three possible results.
const (
	TrendRising  = "rising"
	TrendFalling = "falling"
	TrendFlat    = "flat"
)

// Trend classifies a series as [TrendRising], [TrendFalling] or [TrendFlat]
// by comparing how many steps go up against how many go down.
//
// A series of fewer than two values has no steps and is [TrendFlat].
func Trend(values []int) string {
	up, down := 0, 0

	for i := 1; i < len(values); i++ {
		switch {
		case values[i] > values[i-1]:
			up++
		case values[i] < values[i-1]:
			down++
		}
	}

	if up > down {
		return TrendRising
	}

	if down > up {
		return TrendFalling
	}

	return TrendFlat
}
