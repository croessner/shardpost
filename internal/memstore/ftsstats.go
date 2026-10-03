package memstore

import (
	"shardpost/internal/fts"
	"shardpost/internal/fts/census"
	"shardpost/internal/model"
)

var _ model.FTSStatser = (*User)(nil)

// FTSStats implements model.FTSStatser over the term map: one layout 2
// "object" holds every term, the counts are always exact.
func (u *User) FTSStats(req model.FTSStatsRequest) (model.FTSStats, error) {
	u.mu.Lock()
	defer u.mu.Unlock()

	st := model.FTSStats{Shape: model.FTSLayoutShape{Layout: model.FTSLayout2}, Exact: true}
	docs := map[string]bool{}

	var c *census.Census
	if req.Classes {
		c = census.New()
	}

	for term, guids := range u.terms {
		st.Terms++
		st.DictKeys += int64(fts.DictKeys(term, 0))

		for g := range guids {
			docs[g] = true
		}

		if c != nil {
			c.Add(term)
		}
	}

	st.Docs = int64(len(docs))
	st.Objects = []model.FTSObjectStats{{Object: "terms", Kind: model.FTSObjectPostings, Keys: st.Terms}}

	if c != nil {
		st.Classes = c.Result()
	}

	return st, nil
}
