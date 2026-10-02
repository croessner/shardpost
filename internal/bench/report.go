package bench

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"slices"
)

// PoolStats are a pool's statistics as the cluster reports them (all
// namespaces; updated by the OSDs every few seconds).
type PoolStats struct {
	Objects uint64 `json:"objects"`
	Bytes   uint64 `json:"bytes"`
	Reads   uint64 `json:"reads"`
	Writes  uint64 `json:"writes"`
}

// ProbeReport is the cost of deliveries into populated accounts.
type ProbeReport struct {
	Accounts int `json:"accounts"`
	// Parallel deliveries went into one account at a time.
	Parallel   int   `json:"parallel"`
	Deliveries int64 `json:"deliveries"`
	// Latency and PerSecond are of the round without the objecter log
	// (PerSecond over its wall time); CASRetries counts both rounds,
	// per_delivery dividing by their deliveries together.
	Latency    LatencySummary `json:"latency"`
	PerSecond  float64        `json:"deliveries_per_second"`
	CASRetries CASSummary     `json:"cas_retries"`
	// Ops are the operations per delivery by pool and class, counted in
	// a second round of the same size with the objecter log on.
	Ops   map[string]map[string]OpRate `json:"ops_per_delivery"`
	Total OpRate                       `json:"ops_per_delivery_total"`
}

// RoundReport is the cost of one kind of call into the probe accounts
// (an expunge, a search query).
type RoundReport struct {
	What     string `json:"what"`
	Accounts int    `json:"accounts"`
	// Count is the calls of one round.
	Count int64 `json:"count"`
	// Latency is of the round without the objecter log; CASRetries
	// counts both rounds, per_delivery dividing by their calls together
	// (a value per call).
	Latency    LatencySummary               `json:"latency"`
	CASRetries CASSummary                   `json:"cas_retries"`
	Ops        map[string]map[string]OpRate `json:"ops_per_call"`
	Total      OpRate                       `json:"ops_per_call_total"`
}

// SearchReport is the search probe: its cost per query and what every
// query found.
type SearchReport struct {
	RoundReport

	Queries []SearchQueryResult `json:"queries"`
	// Fingerprint covers the results of all queries in all probe
	// accounts; two runs on unchanged accounts give the same one.
	Fingerprint string `json:"fingerprint"`
	// Mismatch is set when the traced round found something else than
	// the first round (the accounts changed in between).
	Mismatch bool `json:"mismatch,omitempty"`
}

// OpenReport is the cost of opening an account.
type OpenReport struct {
	What    string                       `json:"what"`
	Count   int                          `json:"count"`
	Latency LatencySummary               `json:"latency"`
	Ops     map[string]map[string]OpRate `json:"ops_per_open"`
	Total   OpRate                       `json:"ops_per_open_total"`
	// HeapBytes is the heap one open view holds (User() only).
	HeapBytes int64 `json:"heap_bytes_per_view,omitempty"`
}

// Extrapolation scales the per-account means to a number of accounts.
type Extrapolation struct {
	Accounts     int64   `json:"accounts"`
	IndexObjects float64 `json:"index_objects"`
	IndexOmap    float64 `json:"index_omap_bytes"`
	IndexKeys    float64 `json:"index_omap_keys"`
	DataObjects  float64 `json:"data_objects"`
	DataBytes    float64 `json:"data_bytes"`
	DataRawBytes float64 `json:"data_raw_bytes_estimate"`
}

// MeasureReport is the result of a measurement.
type MeasureReport struct {
	Prefix    string `json:"prefix"`
	IndexPool string `json:"index_pool"`
	DataPool  string `json:"data_pool"`
	// SystemPool is the bench system pool (empty: the store's own
	// namespaces live in the index pool).
	SystemPool string `json:"system_pool,omitempty"`
	FTSShards  int    `json:"fts_shards"`
	ShardSize uint32 `json:"mbox_shard_size"`
	ChunkSize int64  `json:"chunk_size"`

	Inventory InventorySummary `json:"inventory"`
	// StoreWide is what lies outside the accounts (~accounts and the
	// other namespaces of the store) in the bench pools.
	StoreWide map[string]PoolUsage `json:"store_wide"`
	// DataRaw estimates the raw space of the inventoried accounts' data
	// objects in an erasure coded pool.
	DataRaw      ECEstimate           `json:"data_raw"`
	Pools        map[string]PoolStats `json:"pools"`
	Extrapolated *Extrapolation       `json:"extrapolated,omitempty"`
	Delivery     *ProbeReport         `json:"delivery,omitempty"`
	// Contention is the delivery probe with several deliveries into one
	// account at a time (measure --contention).
	Contention *ProbeReport `json:"contention,omitempty"`
	// Search and Expunge are the probes of measure --search and
	// --expunge.
	Search      *SearchReport `json:"search,omitempty"`
	Expunge     *RoundReport  `json:"expunge,omitempty"`
	// Convert is the probe of measure --convert: the first round
	// converts the probe accounts to layout 3 (its latency is the time
	// the account is locked, the 451 window of a fts_layout job), the
	// counted second round reshards them once more; SearchAfter repeats
	// the search probe after it, whose fingerprint must equal Search's.
	Convert     *RoundReport  `json:"convert,omitempty"`
	SearchAfter *SearchReport `json:"search_after_convert,omitempty"`
	UserOpen    *OpenReport   `json:"user_open,omitempty"`
	SessionOpen *OpenReport   `json:"session_open,omitempty"`
	Notes       []string      `json:"notes,omitempty"`
}

// Extrapolate scales the inventory to n accounts.
func (r *MeasureReport) Extrapolate(n int64) {
	if n <= 0 || r.Inventory.Accounts == 0 {
		return
	}

	f := float64(n)
	idx := r.Inventory.PerAccount[r.IndexPool][ClassTotal]
	data := r.Inventory.PerAccount[r.DataPool][ClassTotal]
	e := &Extrapolation{Accounts: n,
		IndexObjects: idx.Objects.Mean * f, IndexOmap: idx.OmapBytes.Mean * f, IndexKeys: idx.OmapKeys.Mean * f,
		DataObjects: data.Objects.Mean * f, DataBytes: data.Bytes.Mean * f}
	e.DataRawBytes = float64(r.DataRaw.Raw) / float64(r.Inventory.Accounts) * f
	r.Extrapolated = e
}

// WriteJSON writes v as indented JSON to path ("-": w).
func WriteJSON(w io.Writer, path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}

	b = append(b, '\n')

	if path == "-" {
		_, err = w.Write(b)
		return err
	}

	return os.WriteFile(path, b, 0o644)
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}

	slices.Sort(keys)

	return keys
}

// human formats a byte count.
func human(n int64) string {
	const unit = 1024

	if n < unit {
		return fmt.Sprintf("%d B", n)
	}

	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}

	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// Rates converts counts to rates per n.
func Rates(c OpCounts, n int64) (map[string]map[string]OpRate, OpRate) {
	out := map[string]map[string]OpRate{}

	for pool, classes := range c {
		out[pool] = map[string]OpRate{}
		for class, v := range classes {
			out[pool][class] = v.Per(n)
		}
	}

	return out, c.Total().Per(n)
}
