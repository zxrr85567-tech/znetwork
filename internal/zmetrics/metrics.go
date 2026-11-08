// Package zmetrics exposes node and chain metrics in two forms:
//
//   - a JSON snapshot over JSON-RPC (z_metrics), served by zrpc
//   - the Prometheus text exposition format (version 0.0.4), served by znd at
//     /metrics, so a standard scraper can collect it without a client library
//
// The zero-dependency policy is why the exposition format is written by hand; it
// is a stable, fully specified text format and needs no third-party client.
package zmetrics

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
)

// Registry holds process-wide counters and gauges. It is safe for concurrent use.
type Registry struct {
	mu     sync.RWMutex
	gauges map[string]func() float64
	labels map[string]string

	txAccepted   atomic.Uint64
	txRejected   atomic.Uint64
	blocksApp    atomic.Uint64
	votesCast    atomic.Uint64
	votesRcvd    atomic.Uint64
	roundsAdv    atomic.Uint64
	heightsCmp   atomic.Uint64
	rpcErrors    atomic.Uint64
	peersDropped atomic.Uint64
	startUnix    int64
}

// New returns an empty registry.
func New() *Registry {
	return &Registry{gauges: map[string]func() float64{}, labels: map[string]string{}}
}

// SetLabel attaches a constant label to every sample (e.g. chain_id).
func (r *Registry) SetLabel(name, value string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.labels[name] = value
}

// Gauge registers a callback evaluated at scrape time.
func (r *Registry) Gauge(name, help string, fn func() float64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.gauges[name] = fn
}

// Counter increments.
func (r *Registry) IncTxAccepted()   { r.txAccepted.Add(1) }
func (r *Registry) IncTxRejected()   { r.txRejected.Add(1) }
func (r *Registry) IncBlockApplied() { r.blocksApp.Add(1) }
func (r *Registry) IncVoteCast()     { r.votesCast.Add(1) }
func (r *Registry) IncVoteReceived() { r.votesRcvd.Add(1) }
func (r *Registry) IncRoundAdvanced() {
	r.roundsAdv.Add(1)
}
func (r *Registry) IncHeightCompleted() { r.heightsCmp.Add(1) }
func (r *Registry) IncRPCError()        { r.rpcErrors.Add(1) }
func (r *Registry) IncPeerDropped()     { r.peersDropped.Add(1) }

// Counters returns the raw counter values.
type Counters struct {
	TxAccepted     uint64 `json:"tx_accepted"`
	TxRejected     uint64 `json:"tx_rejected"`
	BlocksApplied  uint64 `json:"blocks_applied"`
	VotesCast      uint64 `json:"votes_cast"`
	VotesReceived  uint64 `json:"votes_received"`
	RoundsAdvanced uint64 `json:"rounds_advanced"`
	HeightsDone    uint64 `json:"heights_completed"`
	RPCErrors      uint64 `json:"rpc_errors"`
	PeersDropped   uint64 `json:"peers_dropped"`
}

// Snapshot reads all counters atomically enough for monitoring purposes.
func (r *Registry) Snapshot() Counters {
	return Counters{
		TxAccepted:     r.txAccepted.Load(),
		TxRejected:     r.txRejected.Load(),
		BlocksApplied:  r.blocksApp.Load(),
		VotesCast:      r.votesCast.Load(),
		VotesReceived:  r.votesRcvd.Load(),
		RoundsAdvanced: r.roundsAdv.Load(),
		HeightsDone:    r.heightsCmp.Load(),
		RPCErrors:      r.rpcErrors.Load(),
		PeersDropped:   r.peersDropped.Load(),
	}
}

type sample struct {
	name, help, typ string
	value           float64
}

// WritePrometheus renders the registry in the Prometheus text format.
func (r *Registry) WritePrometheus() []byte {
	var b strings.Builder
	r.mu.RLock()
	labelPart := ""
	names := make([]string, 0, len(r.labels))
	for k := range r.labels {
		names = append(names, k)
	}
	sort.Strings(names)
	if len(names) > 0 {
		var parts []string
		for _, k := range names {
			parts = append(parts, fmt.Sprintf("%s=%q", k, r.labels[k]))
		}
		labelPart = "{" + strings.Join(parts, ",") + "}"
	}
	gauges := make([]string, 0, len(r.gauges))
	for k := range r.gauges {
		gauges = append(gauges, k)
	}
	fns := make(map[string]func() float64, len(r.gauges))
	for k, v := range r.gauges {
		fns[k] = v
	}
	r.mu.RUnlock()
	sort.Strings(gauges)

	c := r.Snapshot()
	samples := []sample{
		{"znet_tx_accepted_total", "Transactions accepted into the mempool.", "counter", float64(c.TxAccepted)},
		{"znet_tx_rejected_total", "Transactions rejected before reaching the mempool.", "counter", float64(c.TxRejected)},
		{"znet_blocks_applied_total", "Blocks applied to the local chain.", "counter", float64(c.BlocksApplied)},
		{"znet_votes_cast_total", "Consensus votes produced by this node.", "counter", float64(c.VotesCast)},
		{"znet_votes_received_total", "Consensus votes received from peers.", "counter", float64(c.VotesReceived)},
		{"znet_rounds_advanced_total", "Consensus round timeouts.", "counter", float64(c.RoundsAdvanced)},
		{"znet_heights_completed_total", "Heights completed by this node.", "counter", float64(c.HeightsDone)},
		{"znet_rpc_errors_total", "JSON-RPC calls that returned an error.", "counter", float64(c.RPCErrors)},
		{"znet_peers_dropped_total", "Peer connections dropped.", "counter", float64(c.PeersDropped)},
	}
	for _, s := range samples {
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s %s\n%s%s %s\n",
			s.name, s.help, s.name, s.typ, s.name, labelPart, formatFloat(s.value))
	}
	for _, name := range gauges {
		fmt.Fprintf(&b, "# TYPE %s gauge\n%s%s %s\n", name, name, labelPart, formatFloat(fns[name]()))
	}
	return []byte(b.String())
}

func formatFloat(f float64) string {
	if f == float64(int64(f)) {
		return fmt.Sprintf("%d", int64(f))
	}
	return fmt.Sprintf("%g", f)
}
