package session

import (
	"fmt"
	"math"
)

// AddUsage builds the next value before installation: overflow must not partially update
// accounting or consume the turn sequence.
func AddUsage(total, delta Usage) (Usage, error) {
	next := total
	pairs := []struct {
		dst   *int64
		delta int64
	}{
		{&next.InputTokens, delta.InputTokens}, {&next.OutputTokens, delta.OutputTokens},
		{&next.CachedTokens, delta.CachedTokens}, {&next.TotalTokens, delta.TotalTokens},
		{&next.TurnCount, delta.TurnCount},
	}
	for _, p := range pairs {
		if p.delta < 0 || *p.dst < 0 || p.delta > math.MaxInt64-*p.dst {
			return total, fmt.Errorf("session: usage counter overflow")
		}
		*p.dst += p.delta
	}
	cache := []struct {
		dst   *int
		delta int
	}{
		{&next.Cache.InputTokens, delta.Cache.InputTokens}, {&next.Cache.CachedTokens, delta.Cache.CachedTokens},
		{&next.Cache.ReportedInputTokens, delta.Cache.ReportedInputTokens},
	}
	for _, p := range cache {
		if p.delta < 0 || *p.dst < 0 || p.delta > math.MaxInt-*p.dst {
			return total, fmt.Errorf("session: cache usage counter overflow")
		}
		*p.dst += p.delta
	}
	next.Cache.Tracking = total.Cache.Tracking || delta.Cache.Tracking
	next.Cache.UnknownHistory = total.Cache.UnknownHistory || delta.Cache.UnknownHistory
	return next, nil
}
