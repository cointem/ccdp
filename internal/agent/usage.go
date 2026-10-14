package agent

import (
	"fmt"
	"math"

	"ccdp/internal/protocol"
	"ccdp/internal/session"
)

func usageFromSession(u session.Usage) (Usage, error) {
	for _, v := range []int64{u.InputTokens, u.OutputTokens, u.CachedTokens, u.TurnCount} {
		if v < 0 || int64(int(v)) != v {
			return Usage{}, fmt.Errorf("agent: usage counter %d is outside runtime range", v)
		}
	}
	return Usage{InputTokens: int(u.InputTokens), OutputTokens: int(u.OutputTokens), CachedTokens: int(u.CachedTokens), TurnCount: int(u.TurnCount), Cache: u.Cache}, nil
}

// addRuntimeUsage uses the same checked reducer as replay. Build a candidate
// before committing so overflow never lands in the journal or live projection.
func addRuntimeUsage(total Usage, delta session.Usage, includeCache bool) (Usage, error) {
	if !includeCache {
		delta.Cache = protocol.CacheStats{}
	}
	next, err := session.AddUsage(session.Usage{
		InputTokens: int64(total.InputTokens), OutputTokens: int64(total.OutputTokens),
		CachedTokens: int64(total.CachedTokens), TurnCount: int64(total.TurnCount),
		TotalTokens: int64(total.InputTokens) + int64(total.OutputTokens), Cache: total.Cache,
	}, delta)
	if err != nil {
		return total, err
	}
	if next.InputTokens > math.MaxInt64-next.OutputTokens {
		return total, fmt.Errorf("agent: total usage counter overflow")
	}
	return usageFromSession(next)
}
