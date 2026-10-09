package bank

// ConsolidationDue reports how many of a bank's facts are waiting to be
// consolidated, counting only banks that would consolidate on their own: one
// whose consolidation setting is off, or that has no model to do it with,
// reports zero. A dream uses it to catch the banks whose retain-time trigger
// was missed (a worker that was down, a model that was unreachable) without
// overriding a bank that asked not to be consolidated.
func (e *Engine) ConsolidationDue(bankID string) int {
	if !e.AI.Available() {
		return 0
	}
	p, err := e.Profile(bankID)
	if err != nil || e.consolidationMode(p) != "auto" {
		return 0
	}
	return e.pendingConsolidation(bankID)
}
