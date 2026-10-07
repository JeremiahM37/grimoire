package bank

// BankStats is a bank's size and state at a glance.
type BankStats struct {
	BankID               string         `json:"bank_id"`
	Facts                int            `json:"facts"`
	FactsByType          map[string]int `json:"facts_by_type"`
	HumanFacts           int            `json:"human_facts"`
	Observations         int            `json:"observations"`
	Documents            int            `json:"documents"`
	Entities             int            `json:"entities"`
	MentalModels         int            `json:"mental_models"`
	PendingConsolidation int            `json:"pending_consolidation"`
	Operations           map[string]int `json:"operations_by_status"`
	Consolidation        string         `json:"consolidation"`
	ModelAvailable       bool           `json:"model_available"`
}

// Stats summarises a bank.
func (e *Engine) Stats(bankID string) (*BankStats, error) {
	p, err := e.Profile(bankID)
	if err != nil {
		return nil, err
	}
	c, err := e.cache(bankID)
	if err != nil {
		return nil, err
	}
	s := &BankStats{BankID: bankID, FactsByType: map[string]int{}, Entities: len(c.entities),
		Operations: e.OperationCounts(bankID), Consolidation: e.consolidationMode(p), ModelAvailable: e.AI.Available()}
	for i := range c.units {
		u := &c.units[i]
		if u.Type == "observation" {
			s.Observations++
			continue
		}
		s.Facts++
		s.FactsByType[u.Type]++
		if u.Human {
			s.HumanFacts++
		}
	}
	s.Documents, _ = e.Index.DB.Count("SELECT COUNT(*) FROM bank_documents WHERE bank=?", bankID)
	s.MentalModels, _ = e.Index.DB.Count("SELECT COUNT(*) FROM bank_models WHERE bank=?", bankID)
	s.PendingConsolidation = e.pendingConsolidation(bankID)
	return s, nil
}
