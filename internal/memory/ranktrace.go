package memory

type searchTrace struct {
	rows map[string]*RankFact
}

// RankFact records ranking facts for one candidate.
type RankFact struct {
	FTSRank, VectorRank  int
	VectorScore          float64
	Base                 float64
	StatusFactor         float64
	ProjectMatch         bool
	RowProject           string
	ScopeMatched         bool
	KeywordReserved      bool
	TookSlotFrom         string
	DisplacedBy          string
	FloorDropped         bool
	FloorScore           float64
	Decay, AgeDays       float64
	SupersedePenalty     int
	NearDuplicatePenalty int
	SupersededBy         []string
	NearDuplicateOf      []string
}

func newSearchTrace() *searchTrace {
	return &searchTrace{rows: map[string]*RankFact{}}
}

func (tr *searchTrace) row(id string) *RankFact {
	if tr == nil {
		return nil
	}
	c, ok := tr.rows[id]
	if !ok {
		c = &RankFact{
			FTSRank: -1, VectorRank: -1, VectorScore: -1,
			StatusFactor: 1.0, ScopeMatched: true,
		}
		tr.rows[id] = c
	}
	return c
}

const maxScopeKeys = 16

// ClampScopeKeys returns a bounded copy of scope keys and the original total.
func ClampScopeKeys(keys []string) ([]string, int) {
	n := min(len(keys), maxScopeKeys)
	out := make([]string, n)
	copy(out, keys[:n])
	return out, len(keys)
}

// ClampAttribution returns a bounded copy of attributed ids.
func ClampAttribution(ids []string) []string {
	const maxAttributed = 8
	if len(ids) == 0 {
		return nil
	}
	n := min(len(ids), maxAttributed)
	out := make([]string, n)
	copy(out, ids[:n])
	return out
}
