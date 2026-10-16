package query

import "github.com/lr00rl/ccli/internal/pricing"

// modelName merges the names Claude Code records for one model ("[1m]"
// variants, dated IDs) into the price-table name.
func modelName(m string) string {
	if n := pricing.Normalize(m); n != "" {
		return n
	}
	return m
}
