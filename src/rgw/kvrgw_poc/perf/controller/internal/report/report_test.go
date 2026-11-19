package report

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"kvrgw-perf/internal/config"
	"kvrgw-perf/internal/results"
)

// fixture writes a results dir: benchmark 1000s..1020s, 1s windows ending at 1001..1020
// (plus one stray row after the end). ops/sec == window number * 100 so exclusion is visible.
func fixture(t *testing.T, failed bool) string {
	dir := t.TempDir()
	w := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	const start = int64(1_000_000)
	w("events.csv", fmt.Sprintf("unix_ts_ms,event\n%d,invocation_start\n%d,benchmark_start\n%d,benchmark_end\n%d,verify_start\n",
		start-500, start, start+20000, start+21000))
	var m, h, s, c strings.Builder
	m.WriteString(strings.Join(results.MetricsHeader, ",") + "\n")
	h.WriteString("window,op_type,bucket_index,count\n")
	s.WriteString("unix_ts_ms,window,name,value\n")
	c.WriteString("unix_ts_ms,source,cores\n")
	for win := int64(0); win < 22; win++ { // windows 20,21 end after benchmark_end
		ts := start + (win+1)*1000
		rate := (win + 1) * 100
		fmt.Fprintf(&m, "%d,%d,put_object,%d,0,%d.000,1000.000,1000,2000,3000,4000,5000,%d,0\n", ts, win, rate, rate, rate)
		// 100 samples of ~1000us per window (bucket 100 holds 1000us), win 21 would be excluded
		fmt.Fprintf(&h, "%d,put_object,%d,%d\n", win, 100, rate)
		fmt.Fprintf(&s, "%d,%d,PutObject.count,%d\n", ts, win, (win+1)*100)
		fmt.Fprintf(&s, "%d,%d,PutObject.total_us,%d\n", ts, win, (win+1)*100*500)
		fmt.Fprintf(&s, "%d,%d,PutObject.fdb_us,%d\n", ts, win, (win+1)*100*400)
		fmt.Fprintf(&c, "%d,worker-0,%d.0\n", ts, 2)
		fmt.Fprintf(&c, "%d,system,%d.0\n", ts, 10)
	}
	w("metrics.csv", m.String())
	w("hist.csv", h.String())
	w("server_stats.csv", s.String())
	w("cpu.csv", c.String())
	// FDB: 3 storage servers (one hotspot), 1 log
	var fc, fp strings.Builder
	fc.WriteString(strings.Join(results.FDBClusterHeader, ",") + "\n")
	fp.WriteString(strings.Join(results.FDBProcessHeader, ",") + "\n")
	for i := int64(0); i < 20; i++ {
		ts := start + i*1000
		fmt.Fprintf(&fc, "%d,4000,900,38000,0.5,12000000,5000000,3000000,0.4\n", ts)
		for _, ss := range []struct {
			addr string
			cpu  string
			in   string
		}{{"10.0.0.1:4501", "0.5", "1000"}, {"10.0.0.2:4502", "0.6", "1100"}, {"10.0.0.3:4503", "0.97", "3000"}} {
			fmt.Fprintf(&fp, "%d,%s,storage,%s,100,100,%s,1000,1,1,0.3,0.5,5,,0.1,1,2\n", ts, ss.addr, ss.cpu, ss.in)
		}
		fmt.Fprintf(&fp, "%d,10.0.0.9:4600,log,0.5,100,100,70,69,,,,,1000,50,0.1,1,2\n", ts)
		fmt.Fprintf(&fp, "%d,10.0.0.8:4700,\"commit_proxy,resolver\",0.2,100,100,,,,,,,,,0,0,0\n", ts)
	}
	w("fdb_cluster.csv", fc.String())
	w("fdb_process.csv", fp.String())
	sum := results.Summary{Command: "run-put-object", Parameters: config.Defaults(), Failed: failed, OpErrors: 6,
		Verify: &results.VerifySummary{Ran: true, Pass: true, DurationMs: 41000, Buckets: make([]results.BucketVerify, 4)}}
	sum.Parameters.ReportIntervalMs = 1000
	sum.Parameters.WorkerCount, sum.Parameters.ThreadsPerWorker, sum.Parameters.NumBuckets = 4, 8, 4
	b, _ := json.Marshal(sum)
	w("summary.json", string(b))
	return dir
}

func render(t *testing.T, dir string, o Options) (string, int) {
	var buf bytes.Buffer
	code := Run(&buf, dir, o)
	return buf.String(), code
}

func TestWarmupWindowing(t *testing.T) {
	dir := fixture(t, false)
	out, code := render(t, dir, Options{Warmup: 10 * time.Second})
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	// Windows ending in (start+10s, start+20s] => window numbers 10..19 => rates 1100..2000.
	// Excluded: rows before warmup (rate<=1000) and after benchmark_end (windows 20, 21).
	if !strings.Contains(out, "1,100") || strings.Contains(out, "2,100") {
		t.Fatalf("min/max rates wrong:\n%s", out)
	}
	if !strings.Contains(out, "10 windows of 1s") {
		t.Fatalf("window count:\n%s", out)
	}
	if !strings.Contains(out, "Warmup skipped: first 10s") {
		t.Fatal("warmup in header")
	}
	// Totals: sum 1100..2000 = 15500 ops from merged hist windows only.
	if !strings.Contains(out, "Totals: 15,500 ops") {
		t.Fatalf("totals:\n%s", out)
	}
	if !strings.Contains(out, "Verify    : PASS  (4 buckets") {
		t.Fatalf("verify line:\n%s", out)
	}
	// server stats diffed over the window: 100 ops per window x 10 windows... count delta = 2000-1000.
	if !strings.Contains(out, "avg_total_us") || !strings.Contains(out, "PutObject            1,000           500") ||
		strings.Contains(out, "avg_fdb") || strings.Contains(out, "commit=") {
		t.Fatalf("server avg:\n%s", out)
	}
	// worker CPU and system CPU present
	if !strings.Contains(out, "worker 0") || !strings.Contains(out, "system (host)") {
		t.Fatalf("cpu section:\n%s", out)
	}
}

func TestNoWarmupIncludesAllWindows(t *testing.T) {
	out, _ := render(t, fixture(t, false), Options{})
	if !strings.Contains(out, "20 windows of 1s") || !strings.Contains(out, "Totals: 21,000 ops") {
		t.Fatalf("\n%s", out)
	}
}

func TestDevPercent(t *testing.T) {
	// ops/sec series 1100..2000 (10 windows): median 1550; |v-med|/med>10% for most values.
	out, _ := render(t, fixture(t, false), Options{Warmup: 10 * time.Second, Deviation: 10})
	var line string
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), "ops/sec") {
			line = l
		}
	}
	// values within 10% of 1550 are 1395..1705: 1400,1500,1600,1700 => 4 of 10 => 60% deviate
	if !strings.Contains(line, "60.0%") {
		t.Fatalf("ops/sec line %q", line)
	}
	out, _ = render(t, fixture(t, false), Options{Warmup: 10 * time.Second, Deviation: 100})
	if strings.Contains(out, "60.0%") {
		t.Fatal("deviation flag ignored")
	}
}

func TestRoleAggregationVsPerServer(t *testing.T) {
	dir := fixture(t, false)
	agg, _ := render(t, dir, Options{Warmup: 5 * time.Second})
	if !strings.Contains(agg, "SS  (3)") || !strings.Contains(agg, "LOG (1)") || !strings.Contains(agg, "SL  (1)") {
		t.Fatalf("aggregated roles:\n%s", agg)
	}
	if !strings.Contains(agg, "cpu=0.50-0.97") {
		t.Fatalf("range:\n%s", agg)
	}
	byRole := strings.Split(strings.Split(agg, "--- FDB by role ---")[1], "--- Health ---")[0]
	if strings.Contains(byRole, "10.0.0.3:4503") {
		t.Fatal("aggregated view must not list processes")
	}
	per, _ := render(t, dir, Options{Warmup: 5 * time.Second, PerServer: true})
	if !strings.Contains(per, "10.0.0.3:4503") || !strings.Contains(per, "[HOTSPOT]") || !strings.Contains(per, "10.0.0.1:4501") {
		t.Fatalf("per server:\n%s", per)
	}
	for _, l := range strings.Split(per, "\n") {
		if strings.Contains(l, "SS  10.0.0.1:4501") && strings.Contains(l, "[HOTSPOT]") {
			t.Fatal("only the hot server should be flagged")
		}
	}
	if !strings.Contains(agg, "[OK] TLog queue stable") {
		t.Fatalf("health:\n%s", agg)
	}
}

func TestExitCodes(t *testing.T) {
	if _, code := render(t, fixture(t, true), Options{}); code != 1 {
		t.Fatalf("failed run => 1, got %d", code)
	}
	if _, code := render(t, filepath.Join(t.TempDir(), "missing"), Options{}); code != 2 {
		t.Fatalf("unreadable => 2, got %d", code)
	}
	empty := t.TempDir()
	if _, code := render(t, empty, Options{}); code != 2 {
		t.Fatalf("no events.csv => 2, got %d", code)
	}
}
