package adherence

import "time"

// RowsSince returns every injection newer than since, oldest first. Memory
// replay (internal/replay) reads outcomes from it; it changes nothing.
func (s *Store) RowsSince(since time.Time) ([]Row, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rs, err := s.db.Query(`SELECT `+rowCols+` FROM injections WHERE ts>=? ORDER BY id`, since.Unix())
	if err != nil {
		return nil, err
	}
	return scanRows(rs)
}
