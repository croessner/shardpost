package bench

import (
	"math"
	"slices"
)

// Usage is what objects of one class take: their number, their data
// bytes (object size) and their omap (keys, and bytes of keys and
// values; RocksDB holds it on the OSD).
type Usage struct {
	Objects   int64 `json:"objects"`
	Bytes     int64 `json:"bytes"`
	OmapKeys  int64 `json:"omap_keys"`
	OmapBytes int64 `json:"omap_bytes"`
}

// Add adds u2 to u.
func (u *Usage) Add(u2 Usage) {
	u.Objects += u2.Objects
	u.Bytes += u2.Bytes
	u.OmapKeys += u2.OmapKeys
	u.OmapBytes += u2.OmapBytes
}

// PoolUsage is the usage by object class of one pool.
type PoolUsage map[string]*Usage

// Add adds usage to a class.
func (p PoolUsage) Add(class string, u Usage) {
	if p[class] == nil {
		p[class] = &Usage{}
	}

	p[class].Add(u)
}

// Total sums the classes.
func (p PoolUsage) Total() Usage {
	var t Usage

	for _, u := range p {
		t.Add(*u)
	}

	return t
}

// AccountUsage is the usage of one account, by pool.
type AccountUsage struct {
	Account  string               `json:"account"`
	Messages int64                `json:"messages"`
	Pools    map[string]PoolUsage `json:"pools"`
}

// Spread is the mean, minimum and maximum of a value over the accounts.
type Spread struct {
	Mean float64 `json:"mean"`
	Min  int64   `json:"min"`
	Max  int64   `json:"max"`
}

// ClassSpread is the per-account spread of a class's usage.
type ClassSpread struct {
	Objects   Spread `json:"objects"`
	Bytes     Spread `json:"bytes"`
	OmapKeys  Spread `json:"omap_keys"`
	OmapBytes Spread `json:"omap_bytes"`
}

// InventorySummary is the inventory of the bench accounts per account
// and per message.
type InventorySummary struct {
	Accounts int `json:"accounts"`
	// MessagesPerAccount is the spread of the accounts' message counts.
	MessagesPerAccount Spread `json:"messages_per_account"`
	// PerAccount is the usage per account by pool and class, and the
	// total of each pool under the class "total".
	PerAccount map[string]map[string]ClassSpread `json:"per_account"`
	// PerMessage is the mean usage of a pool divided by the mean
	// message count: what one more message costs, if the cost grows
	// linearly (the full-text shards do not: their count is fixed).
	PerMessage map[string]Usage `json:"per_message"`
}

// ClassTotal is the pseudo-class of a pool's sum.
const ClassTotal = "total"

// Summarize computes the spreads over the accounts.
func Summarize(accts []AccountUsage) InventorySummary {
	s := InventorySummary{Accounts: len(accts), PerAccount: map[string]map[string]ClassSpread{}, PerMessage: map[string]Usage{}}
	if len(accts) == 0 {
		return s
	}

	msgs := make([]int64, len(accts))
	for i, a := range accts {
		msgs[i] = a.Messages
	}

	s.MessagesPerAccount = spread(msgs)

	for pool, classes := range poolClasses(accts) {
		s.PerAccount[pool] = map[string]ClassSpread{}

		for _, class := range classes {
			s.PerAccount[pool][class] = classSpread(accts, pool, class)
		}

		if s.MessagesPerAccount.Mean > 0 {
			t := s.PerAccount[pool][ClassTotal]
			per := func(sp Spread) int64 { return int64(math.Round(sp.Mean / s.MessagesPerAccount.Mean)) }
			s.PerMessage[pool] = Usage{Objects: per(t.Objects), Bytes: per(t.Bytes), OmapKeys: per(t.OmapKeys), OmapBytes: per(t.OmapBytes)}
		}
	}

	return s
}

// poolClasses lists the classes seen in each pool, and ClassTotal.
func poolClasses(accts []AccountUsage) map[string][]string {
	seen := map[string]map[string]bool{}

	for _, a := range accts {
		for pool, classes := range a.Pools {
			if seen[pool] == nil {
				seen[pool] = map[string]bool{ClassTotal: true}
			}

			for class := range classes {
				seen[pool][class] = true
			}
		}
	}

	out := map[string][]string{}

	for pool, classes := range seen {
		for class := range classes {
			out[pool] = append(out[pool], class)
		}

		slices.Sort(out[pool])
	}

	return out
}

// classSpread is the spread of one class (zero where an account has
// none of it).
func classSpread(accts []AccountUsage, pool, class string) ClassSpread {
	var objects, bytes, keys, omap []int64

	for _, a := range accts {
		var u Usage

		if class == ClassTotal {
			u = a.Pools[pool].Total()
		} else if c := a.Pools[pool][class]; c != nil {
			u = *c
		}

		objects = append(objects, u.Objects)
		bytes = append(bytes, u.Bytes)
		keys = append(keys, u.OmapKeys)
		omap = append(omap, u.OmapBytes)
	}

	return ClassSpread{Objects: spread(objects), Bytes: spread(bytes), OmapKeys: spread(keys), OmapBytes: spread(omap)}
}

func spread(v []int64) Spread {
	s := Spread{Min: math.MaxInt64, Max: math.MinInt64}
	sum := 0.0

	for _, x := range v {
		s.Min, s.Max = min(s.Min, x), max(s.Max, x)
		sum += float64(x)
	}

	s.Mean = round3(sum / float64(len(v)))

	return s
}

// ECEstimate estimates the raw space an erasure coded pool allocates for
// objects of the given sizes: each object is cut into DataChunks chunks
// of whole stripe units, CodingChunks more are computed, and every chunk
// takes whole allocation units on its OSD. Small objects therefore cost
// far more than their size (scaling plan B15).
type ECEstimate struct {
	DataChunks   int     `json:"k"`
	CodingChunks int     `json:"m"`
	StripeUnit   int64   `json:"stripe_unit"`
	MinAlloc     int64   `json:"min_alloc_size"`
	Logical      int64   `json:"logical_bytes"`
	Raw          int64   `json:"raw_bytes"`
	Overhead     float64 `json:"overhead"`
}

// Add accounts for one object of size bytes.
func (e *ECEstimate) Add(size int64) {
	k, unit := int64(max(e.DataChunks, 1)), max(e.StripeUnit, 1)
	stripes := max((size+k*unit-1)/(k*unit), 1)
	chunk := stripes * unit

	if a := max(e.MinAlloc, 1); chunk%a != 0 {
		chunk += a - chunk%a
	}

	e.Logical += size
	e.Raw += chunk * int64(e.DataChunks+e.CodingChunks)

	if e.Logical > 0 {
		e.Overhead = round3(float64(e.Raw) / float64(e.Logical))
	}
}
