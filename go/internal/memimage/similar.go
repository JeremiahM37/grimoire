package memimage

import "sort"

// Match is one picture that is visually close to a query picture.
type Match struct {
	SHA           string  `json:"sha"`
	HashDistance  int     `json:"hash_distance"`  // differing dHash bits, 0-64
	ColorDistance float64 `json:"color_distance"` // histogram L1 distance, 0-1
}

// Rank returns the candidates within maxHash differing bits of the query,
// closest first. Ties on the hash are broken by colour distance and then by
// content address, so the order is the same on every call.
//
// Distances are for ordering and thresholding only. A hash distance of 0 means
// the 9x8 luminance pattern is identical, which is strong evidence of the same
// picture; it does not establish that two pictures show the same thing.
func Rank(query Signature, candidates map[string]Signature, maxHash int) []Match {
	if maxHash < 0 {
		maxHash = 0
	}
	if maxHash > 64 {
		maxHash = 64
	}
	out := make([]Match, 0, len(candidates))
	for sha, sig := range candidates {
		d := Hamming(query.DHash, sig.DHash)
		if d > maxHash {
			continue
		}
		out = append(out, Match{SHA: sha, HashDistance: d, ColorDistance: ColorDistance(query.Hist, sig.Hist)})
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.HashDistance != b.HashDistance {
			return a.HashDistance < b.HashDistance
		}
		if a.ColorDistance != b.ColorDistance {
			return a.ColorDistance < b.ColorDistance
		}
		return a.SHA < b.SHA
	})
	return out
}
