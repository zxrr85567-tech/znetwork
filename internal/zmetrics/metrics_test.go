package zmetrics

import (
	"strings"
	"sync"
	"testing"
)

func TestCountersAreSafeUnderConcurrency(t *testing.T) {
	r := New()
	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 500; i++ {
				r.IncTxAccepted()
				r.IncVoteReceived()
			}
		}()
	}
	wg.Wait()
	c := r.Snapshot()
	if c.TxAccepted != 4000 || c.VotesReceived != 4000 {
		t.Fatalf("counters = %+v", c)
	}
}

func TestPrometheusExposition(t *testing.T) {
	r := New()
	r.SetLabel("chain_id", "znetwork-devnet-1")
	r.IncBlockApplied()
	r.IncTxRejected()
	r.Gauge("znet_height", "Current chain height.", func() float64 { return 42 })
	out := string(r.WritePrometheus())

	for _, want := range []string{
		"# TYPE znet_blocks_applied_total counter",
		`znet_blocks_applied_total{chain_id="znetwork-devnet-1"} 1`,
		"# TYPE znet_height gauge",
		`znet_height{chain_id="znetwork-devnet-1"} 42`,
		"znet_tx_rejected_total{chain_id=\"znetwork-devnet-1\"} 1",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
	// Every sample must be preceded by its TYPE line, otherwise strict parsers
	// reject the exposition.
	lines := strings.Split(strings.TrimSpace(out), "\n")
	seen := map[string]bool{}
	for _, l := range lines {
		if strings.HasPrefix(l, "# TYPE ") {
			seen[strings.Fields(l)[2]] = true
		} else if l != "" && !strings.HasPrefix(l, "#") {
			name := l
			if i := strings.IndexAny(l, "{"); i > 0 {
				name = l[:i]
			} else if i := strings.Index(l, " "); i > 0 {
				name = l[:i]
			}
			if !seen[name] {
				t.Fatalf("sample %q has no preceding TYPE line", l)
			}
		}
	}
}

func TestGaugeIsEvaluatedAtScrapeTime(t *testing.T) {
	r := New()
	v := 1.0
	r.Gauge("znet_test", "help", func() float64 { return v })
	if !strings.Contains(string(r.WritePrometheus()), "znet_test 1") {
		t.Fatal("gauge not rendered")
	}
	v = 7
	if !strings.Contains(string(r.WritePrometheus()), "znet_test 7") {
		t.Fatal("gauge not re-evaluated")
	}
}

func TestNoLabelsMeansNoBraces(t *testing.T) {
	r := New()
	r.IncTxAccepted()
	out := string(r.WritePrometheus())
	if !strings.Contains(out, "\nznet_tx_accepted_total 1\n") {
		t.Fatalf("unexpected exposition:\n%s", out)
	}
	if strings.Contains(out, "{}") {
		t.Fatal("empty label set rendered as braces")
	}
}
