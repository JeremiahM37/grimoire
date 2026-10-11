package api

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math/rand/v2"
	"strconv"
	"time"

	"github.com/JeremiahM37/grimoire/go/internal/exp"
)

// Exposure logging for the offline ranking learner (experiment "ips",
// docs/lab/ips.md). Every function here is reached only under exp.On("ips"),
// except the field copies in logInjections and logWithheld, which copy zero
// values when the flag is off.

const ipsFlag = "ips"

// maxPositionExplore bounds memory_position_explore. Above half the slates the
// agent would see a mostly shuffled context, which is not the ranking under test.
const maxPositionExplore = 0.5

func ipsOn() bool { return exp.On(ipsFlag) }

// positionExplore is the share of slates whose order is shuffled. It defaults
// to 0, which means every slate keeps its ranking and the position propensity is
// deterministic. The learner then cannot estimate position bias, and says so.
func (s *Server) positionExplore() float64 {
	v, err := strconv.ParseFloat(s.setting("memory_position_explore"), 64)
	if err != nil || v <= 0 {
		return 0
	}
	if v > maxPositionExplore {
		return maxPositionExplore
	}
	return v
}

// ipsPositions gives each item of a logged response its 1-based slate position,
// and shuffles the slate on an explored response. The slate id ties the items
// shown and withheld from one response together for the learner. Positions are
// those of the ranked slate before the holdout, since the holdout removes items
// from the displayed list afterwards.
func (s *Server) ipsPositions(items []contextItem, lg ctxLog) ([]contextItem, ctxLog) {
	sum := sha256.Sum256([]byte(lg.session + "\x00" + lg.query + "\x00" + strconv.FormatInt(time.Now().UnixNano(), 10)))
	lg.slate = hex.EncodeToString(sum[:8])
	explored := false
	if eps := s.positionExplore(); eps > 0 && rand.Float64() < eps {
		explored = true
		rand.Shuffle(len(items), func(i, j int) { items[i], items[j] = items[j], items[i] })
	}
	for i := range items {
		items[i].pos = i + 1
		items[i].explored = explored
	}
	return items, lg
}

// ipsFields returns the exposure columns for one logged item. Zero values when
// the item was not positioned, so flag-off rows keep their defaults.
func ipsFields(lg ctxLog, it contextItem) (pos int, slate string, explored bool, feats string) {
	if it.pos == 0 {
		return 0, "", false, ""
	}
	if it.feat != nil {
		if b, err := json.Marshal(it.feat); err == nil {
			feats = string(b)
		}
	}
	return it.pos, lg.slate, it.explored, feats
}
