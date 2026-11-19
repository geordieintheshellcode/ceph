// Package report implements `kvrgw-perf report <results-dir>`: it reads only the files
// written by an invocation (never touches workers or FDB).
package report

import (
	"encoding/csv"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"kvrgw-perf/internal/results"
	"kvrgw-perf/internal/stats"
)

// Options are the report flags.
type Options struct {
	Warmup    time.Duration
	Deviation float64
	PerServer bool
}

// Exit codes: 0 ok, 1 summary records a failed run, 2 unreadable results dir.
func Run(w io.Writer, dir string, o Options) int {
	if o.Deviation == 0 {
		o.Deviation = 10
	}
	r, err := load(dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "report:", err)
		return 2
	}
	r.print(w, o)
	if r.sum != nil && r.sum.Failed {
		return 1
	}
	return 0
}

type metricRow struct {
	ts        int64
	window    int64
	op        string
	count     uint64
	errors    uint64
	opsPerSec float64
	meanUs    float64
	hasLat    bool
	p50, p99  float64
}

type data struct {
	dir            string
	startMs, endMs int64
	metrics        []metricRow
	hist           map[string]map[int64]stats.Hist // op -> window -> hist
	server         map[string][]sample             // name -> series
	cpu            map[string][]sample
	fdbCluster     map[string][]sample // column -> series
	fdbProc        map[procKey]map[string][]sample
	sum            *results.Summary
	intervalMs     int64
	hasFDB         bool
}

type sample struct {
	ts int64
	v  float64
}

type procKey struct{ addr, role string }

func readCSV(path string) ([]string, [][]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()
	rd := csv.NewReader(f)
	rd.FieldsPerRecord = -1
	recs, err := rd.ReadAll()
	if err != nil {
		return nil, nil, err
	}
	if len(recs) == 0 {
		return nil, nil, nil
	}
	return recs[0], recs[1:], nil
}

func pf(s string) float64 { v, _ := strconv.ParseFloat(s, 64); return v }
func pi(s string) int64   { v, _ := strconv.ParseInt(s, 10, 64); return v }
func pu(s string) uint64  { v, _ := strconv.ParseUint(s, 10, 64); return v }

func load(dir string) (*data, error) {
	d := &data{dir: dir, hist: map[string]map[int64]stats.Hist{}, server: map[string][]sample{},
		cpu: map[string][]sample{}, fdbCluster: map[string][]sample{}, fdbProc: map[procKey]map[string][]sample{}}
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		return nil, fmt.Errorf("cannot read results directory %q", dir)
	}
	// events.csv (required)
	_, ev, err := readCSV(filepath.Join(dir, "events.csv"))
	if err != nil {
		return nil, fmt.Errorf("events.csv: %w", err)
	}
	for _, r := range ev {
		if len(r) < 2 {
			continue
		}
		switch r[1] {
		case "benchmark_start":
			d.startMs = pi(r[0])
		case "benchmark_end":
			d.endMs = pi(r[0])
		}
	}
	if d.startMs == 0 || d.endMs == 0 || d.endMs < d.startMs {
		return nil, fmt.Errorf("events.csv lacks a valid benchmark_start/benchmark_end")
	}
	if s, err := results.ReadSummary(filepath.Join(dir, "summary.json")); err == nil {
		d.sum = s
		d.intervalMs = s.Parameters.ReportIntervalMs
	}
	// metrics.csv
	if _, rows, err := readCSV(filepath.Join(dir, "metrics.csv")); err == nil {
		for _, r := range rows {
			if len(r) < 14 {
				continue
			}
			m := metricRow{ts: pi(r[0]), window: pi(r[1]), op: r[2], count: pu(r[3]), errors: pu(r[4]),
				opsPerSec: pf(r[5])}
			if r[6] != "" {
				m.hasLat = true
				m.meanUs, m.p50, m.p99 = pf(r[6]), pf(r[7]), pf(r[9])
			}
			d.metrics = append(d.metrics, m)
		}
	}
	// hist.csv
	if _, rows, err := readCSV(filepath.Join(dir, "hist.csv")); err == nil {
		for _, r := range rows {
			if len(r) < 4 {
				continue
			}
			op, win := r[1], pi(r[0])
			if d.hist[op] == nil {
				d.hist[op] = map[int64]stats.Hist{}
			}
			h := d.hist[op][win]
			h.AddBucket(int(pi(r[2])), pu(r[3]))
			d.hist[op][win] = h
		}
	}
	if _, rows, err := readCSV(filepath.Join(dir, "server_stats.csv")); err == nil {
		for _, r := range rows {
			if len(r) >= 4 {
				d.server[r[2]] = append(d.server[r[2]], sample{pi(r[0]), pf(r[3])})
			}
		}
	}
	if _, rows, err := readCSV(filepath.Join(dir, "cpu.csv")); err == nil {
		for _, r := range rows {
			if len(r) >= 3 {
				d.cpu[r[1]] = append(d.cpu[r[1]], sample{pi(r[0]), pf(r[2])})
			}
		}
	}
	if hdr, rows, err := readCSV(filepath.Join(dir, "fdb_cluster.csv")); err == nil && hdr != nil {
		d.hasFDB = true
		for _, r := range rows {
			for i := 1; i < len(r) && i < len(hdr); i++ {
				d.fdbCluster[hdr[i]] = append(d.fdbCluster[hdr[i]], sample{pi(r[0]), pf(r[i])})
			}
		}
	}
	if hdr, rows, err := readCSV(filepath.Join(dir, "fdb_process.csv")); err == nil && hdr != nil {
		for _, r := range rows {
			if len(r) < 3 {
				continue
			}
			k := procKey{r[1], r[2]}
			if d.fdbProc[k] == nil {
				d.fdbProc[k] = map[string][]sample{}
			}
			for i := 3; i < len(r) && i < len(hdr); i++ {
				if r[i] != "" {
					d.fdbProc[k][hdr[i]] = append(d.fdbProc[k][hdr[i]], sample{pi(r[0]), pf(r[i])})
				}
			}
		}
	}
	return d, nil
}

// window selection -------------------------------------------------------------------

// bounds returns the analysis window in ms.
func (d *data) bounds(o Options) (from, to int64) {
	return d.startMs + o.Warmup.Milliseconds(), d.endMs
}

func inSamples(s []sample, from, to int64) []float64 {
	var out []float64
	for _, x := range s {
		if x.ts >= from && x.ts <= to {
			out = append(out, x.v)
		}
	}
	return out
}

// formatting ---------------------------------------------------------------------------

func grouped(n int64) string {
	neg := n < 0
	if neg {
		n = -n
	}
	s := strconv.FormatInt(n, 10)
	var out []byte
	for i, c := range []byte(s) {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, c)
	}
	if neg {
		return "-" + string(out)
	}
	return string(out)
}

func num(v float64) string {
	a := math.Abs(v)
	switch {
	case v == 0:
		return "0"
	case a >= 100:
		return grouped(int64(math.Round(v)))
	case a >= 10:
		return fmt.Sprintf("%.1f", v)
	default:
		return fmt.Sprintf("%.2f", v)
	}
}

func dur(us float64) string {
	switch {
	case us >= 1e6:
		return fmt.Sprintf("%.2fs", us/1e6)
	case us >= 1e3:
		return fmt.Sprintf("%.1fms", us/1e3)
	}
	return fmt.Sprintf("%.0fus", us)
}

func clock(ms int64) string { return time.UnixMilli(ms).UTC().Format("15:04:05Z") }

func row(w io.Writer, label string, s stats.Summary, showDev bool) {
	fmt.Fprintf(w, " %-14s%9s%9s%9s%9s%9s%9s%9s", label, num(s.Min), num(s.Max), num(s.Avg), num(s.Median),
		num(s.P90), num(s.P95), num(s.P99))
	if showDev {
		fmt.Fprintf(w, "%7.1f%%", s.DevPct)
	}
	fmt.Fprintln(w)
}

func (d *data) print(w io.Writer, o Options) {
	from, to := d.bounds(o)
	name := filepath.Base(strings.TrimRight(d.dir, "/"))
	line := strings.Repeat("=", 74)
	fmt.Fprintf(w, "%s\n kvrgw-perf report: %s\n%s\n", line, name, line)

	// ---- header ----
	var nwin int
	seenWin := map[int64]bool{}
	for _, m := range d.metrics {
		if m.ts > from && m.ts <= to && !seenWin[m.window] {
			seenWin[m.window] = true
			nwin++
		}
	}
	if d.sum != nil {
		p := d.sum.Parameters
		fmt.Fprintf(w, " Operation : %s      Workers: %d x %d threads   Buckets: %d\n", d.sum.Command, p.WorkerCount, p.ThreadsPerWorker, p.NumBuckets)
	}
	fmt.Fprintf(w, " Benchmark : %s -> %s (%.0fs)   Warmup skipped: first %s\n", clock(d.startMs), clock(d.endMs),
		float64(d.endMs-d.startMs)/1000, o.Warmup)
	fmt.Fprintf(w, " Analysed  : %s -> %s (%d windows of %s)\n", clock(from), clock(to), nwin,
		(time.Duration(d.intervalMs) * time.Millisecond).String())
	if d.sum != nil {
		if v := d.sum.Verify; v != nil && v.Ran {
			st := "PASS"
			if !v.Pass {
				st = "FAIL"
			}
			fmt.Fprintf(w, " Verify    : %s  (%d buckets, %d missing, %d duplicates, took %s)\n", st, len(v.Buckets), v.Missing, v.Duplicate,
				(time.Duration(v.DurationMs) * time.Millisecond).Round(10*time.Millisecond))
		} else {
			fmt.Fprintln(w, " Verify    : not run")
		}
		if d.sum.OpErrors > 0 {
			note := "(all retried OK)"
			if d.sum.Failed {
				note = "(run FAILED)"
			}
			fmt.Fprintf(w, " Errors    : %d %s\n", d.sum.OpErrors, note)
		} else {
			fmt.Fprintln(w, " Errors    : 0")
		}
		if d.sum.Failed {
			fmt.Fprintf(w, " Status    : FAILED  %s\n", strings.Join(d.sum.Failures, "; "))
		}
	}

	d.printClient(w, o, from, to)
	d.printCPU(w, from, to)
	d.printServer(w, from, to)
	if d.hasFDB {
		d.printFDB(w, o, from, to)
	}
}

func (d *data) printClient(w io.Writer, o Options, from, to int64) {
	ops := []string{}
	for _, op := range []string{"put_object", "list_objects", "delete_multi", "keys_listed", "keys_deleted"} {
		for _, m := range d.metrics {
			if m.op == op {
				ops = append(ops, op)
				break
			}
		}
	}
	if len(ops) == 0 {
		return
	}
	fmt.Fprintln(w, "\n--- Client ops (cluster total, merged across workers) ---")
	for _, op := range ops {
		var rate, mean, p50, p99, errs []float64
		var count, errTotal uint64
		wins := map[int64]bool{}
		for _, m := range d.metrics {
			if m.op != op || m.ts <= from || m.ts > to {
				continue
			}
			wins[m.window] = true
			rate = append(rate, m.opsPerSec)
			errs = append(errs, float64(m.errors))
			count += m.count
			errTotal += m.errors
			if m.hasLat && m.count > 0 {
				mean = append(mean, m.meanUs)
				p50 = append(p50, m.p50)
				p99 = append(p99, m.p99)
			}
		}
		if stats.IsCounter(op) {
			fmt.Fprintf(w, " %-14s%9s%9s%9s%9s%9s%9s%9s%8s\n", op, "min", "max", "avg", "median", "p90", "p95", "p99", "dev%")
			row(w, "units/sec", stats.Summarize(rate, o.Deviation), true)
			fmt.Fprintf(w, " Totals: %s units\n", grouped(int64(count)))
			continue
		}
		fmt.Fprintf(w, " %-14s%9s%9s%9s%9s%9s%9s%9s%8s\n", op, "min", "max", "avg", "median", "p90", "p95", "p99", "dev%")
		row(w, "ops/sec", stats.Summarize(rate, o.Deviation), true)
		row(w, "mean_us", stats.Summarize(mean, o.Deviation), true)
		row(w, "p50_us", stats.Summarize(p50, o.Deviation), true)
		row(w, "p99_us", stats.Summarize(p99, o.Deviation), true)
		row(w, "errors/win", stats.Summarize(errs, o.Deviation), true)
		// overall quantiles from merged hist windows inside the analysis window
		var h stats.Hist
		for win := range wins {
			h.Merge(d.hist[op][win])
		}
		fmt.Fprintf(w, " Totals: %s ops (%s errors)   overall p50=%s p99=%s p999=%s\n", grouped(int64(count)), grouped(int64(errTotal)),
			dur(float64(h.Quantile(0.5))), dur(float64(h.Quantile(0.99))), dur(float64(h.Quantile(0.999))))
	}
}

func (d *data) printCPU(w io.Writer, from, to int64) {
	if len(d.cpu) == 0 {
		return
	}
	fmt.Fprintln(w, "\n--- Worker CPU (client side, cores) ---")
	fmt.Fprintf(w, " %-14s%9s%9s%9s%9s\n", "", "min", "max", "avg", "median")
	var srcs []string
	for k := range d.cpu {
		srcs = append(srcs, k)
	}
	sort.Slice(srcs, func(i, j int) bool {
		if (srcs[i] == "system") != (srcs[j] == "system") {
			return srcs[j] == "system"
		}
		return natLess(srcs[i], srcs[j])
	})
	for _, s := range srcs {
		sm := stats.Summarize(inSamples(d.cpu[s], from, to), 10)
		label := strings.Replace(s, "-", " ", 1)
		if s == "system" {
			label = "system (host)"
		}
		fmt.Fprintf(w, " %-14s%9s%9s%9s%9s\n", label, num(sm.Min), num(sm.Max), num(sm.Avg), num(sm.Median))
	}
}

func natLess(a, b string) bool {
	ia, ib := strings.LastIndex(a, "-"), strings.LastIndex(b, "-")
	if ia >= 0 && ib >= 0 && a[:ia] == b[:ib] {
		x, e1 := strconv.Atoi(a[ia+1:])
		y, e2 := strconv.Atoi(b[ib+1:])
		if e1 == nil && e2 == nil {
			return x < y
		}
	}
	return a < b
}

// serverDelta returns the cumulative delta of a counter over the analysis window.
func (d *data) serverDelta(name string, from, to int64) (float64, bool) {
	s := d.server[name]
	if len(s) == 0 {
		return 0, false
	}
	base, last := 0.0, math.NaN()
	for _, x := range s {
		if x.ts <= from {
			base = x.v
		}
		if x.ts <= to {
			last = x.v
		}
	}
	if math.IsNaN(last) {
		return 0, false
	}
	return last - base, true
}

func (d *data) printServer(w io.Writer, from, to int64) {
	if len(d.server) == 0 {
		return
	}
	secs := float64(to-from) / 1000
	fmt.Fprintln(w, "\n--- Server-side KVRGW (summed over workers) ---")
	var opNames []string
	for n := range d.server {
		if op, ok := strings.CutSuffix(n, ".count"); ok && !strings.HasPrefix(n, "batch.") && !strings.HasPrefix(n, "txn.") {
			opNames = append(opNames, op)
		}
	}
	sort.Strings(opNames)
	// The library does not instrument FDB get/commit/scan time (the hooks are never
	// called), so only count, avg_total and, if any is non-zero, avg_disk are reported.
	type opRow struct {
		op                 string
		count, total, disk float64
	}
	var rows []opRow
	showDisk := false
	for _, op := range opNames {
		c, _ := d.serverDelta(op+".count", from, to)
		if c <= 0 {
			continue
		}
		r := opRow{op, c, d.opDelta(op, "total_us", from, to) / c, d.opDelta(op, "disk_us", from, to) / c}
		showDisk = showDisk || r.disk > 0
		rows = append(rows, r)
	}
	if len(rows) > 0 {
		fmt.Fprintf(w, " %-14s%12s%14s", "op", "count", "avg_total_us")
		if showDisk {
			fmt.Fprintf(w, "%13s", "avg_disk_us")
		}
		fmt.Fprintln(w)
		for _, r := range rows {
			fmt.Fprintf(w, " %-14s%12s%14s", r.op, grouped(int64(r.count)), num(r.total))
			if showDisk {
				fmt.Fprintf(w, "%13s", num(r.disk))
			}
			fmt.Fprintln(w)
		}
	}
	if commits, ok := d.serverDelta("batch.commits", from, to); ok && commits > 0 {
		ent, _ := d.serverDelta("batch.entries", from, to)
		wait, _ := d.serverDelta("batch.wait_us_total", from, to)
		q, _ := d.serverDelta("batch.queue_size_total", from, to)
		cp, _ := d.serverDelta("batch.conflict_pushbacks", from, to)
		fmt.Fprintf(w, "\n %-14s%12s%12s%14s%12s%12s\n", "batches", "commits/s", "avg_size", "avg_wait_us", "avg_queue", "pushbacks")
		fmt.Fprintf(w, " %-14s%12s%12.1f%14s%12.1f%12s\n", "", num(commits/secs), ent/commits, num(wait/commits), q/commits, grouped(int64(cp)))
	}
	rt, _ := d.serverDelta("txn.retries", from, to)
	hf, _ := d.serverDelta("txn.hard_failures", from, to)
	var errs []string
	var names []string
	for n := range d.server {
		if strings.HasPrefix(n, "error.") {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	for _, n := range names {
		if v, _ := d.serverDelta(n, from, to); v > 0 {
			errs = append(errs, fmt.Sprintf("%s=%d", strings.TrimPrefix(n, "error."), int64(v)))
		}
	}
	fmt.Fprintf(w, "\n %-14s%12s%14s\n", "txn", "retries", "hard_failures")
	fmt.Fprintf(w, " %-14s%12s%14s\n", "", grouped(int64(rt)), grouped(int64(hf)))
	fmt.Fprintf(w, " server errors: %s\n", orNone(errs))
}

func (d *data) opDelta(op, field string, from, to int64) float64 {
	v, _ := d.serverDelta(op+"."+field, from, to)
	return v
}

func orNone(s []string) string {
	if len(s) == 0 {
		return "none"
	}
	return strings.Join(s, " ")
}

// FDB -------------------------------------------------------------------------------------

type procStat struct {
	key  procKey
	med  map[string]float64
	flag string
}

func (d *data) procStats(from, to int64) []procStat {
	var out []procStat
	for k, cols := range d.fdbProc {
		ps := procStat{key: k, med: map[string]float64{}}
		any := false
		for c, s := range cols {
			v := inSamples(s, from, to)
			if len(v) > 0 {
				any = true
				ps.med[c] = stats.Summarize(v, 10).Median
			}
		}
		if any {
			ps.flag = ssFlags(k.role, ps.med)
			out = append(out, ps)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].key.role != out[j].key.role {
			return out[i].key.role < out[j].key.role
		}
		return out[i].key.addr < out[j].key.addr
	})
	return out
}

// ssFlags are the per-server flags (thresholds from the legacy health check).
func ssFlags(role string, m map[string]float64) string {
	if role != "storage" {
		return ""
	}
	cpu, in, dur, lag := m["cpu_cores"], m["input_hz"], m["durable_hz"], m["data_lag_s"]
	var f []string
	if cpu >= 0.95 {
		f = append(f, "[SATURATED]")
	}
	if cpu > 0.25 && dur > 0 && in/dur > 2.0 {
		f = append(f, "[HOTSPOT]")
	}
	if lag > 5.0 {
		f = append(f, "[FALLING_BEHIND]")
	}
	return strings.Join(f, " ")
}

func roleLabel(role string) string {
	switch role {
	case "storage":
		return "SS "
	case "log":
		return "LOG"
	}
	return "SL "
}

func (d *data) printFDB(w io.Writer, o Options, from, to int64) {
	fmt.Fprintln(w, "\n--- FDB cluster ---")
	fmt.Fprintf(w, " %-22s%9s%9s%9s%9s\n", "", "min", "max", "avg", "median")
	for _, c := range []struct {
		label, col string
		scale      float64
	}{{"txn/s", "txn_hz", 1}, {"reads/s", "reads_hz", 1}, {"writes/s", "writes_hz", 1}, {"conflicts/s", "conflict_hz", 1},
		{"worst SS queue (MB)", "worst_queue_ss", 1e6}, {"durability lag (s)", "durability_lag_s", 1}} {
		v := inSamples(d.fdbCluster[c.col], from, to)
		for i := range v {
			v[i] /= c.scale
		}
		s := stats.Summarize(v, o.Deviation)
		fmt.Fprintf(w, " %-22s%9s%9s%9s%9s\n", c.label, num(s.Min), num(s.Max), num(s.Avg), num(s.Median))
	}

	procs := d.procStats(from, to)
	fmt.Fprintln(w, "\n--- FDB by role ---")
	if o.PerServer {
		for _, p := range procs {
			m := p.med
			switch p.key.role {
			case "storage":
				r := 0.0
				if m["durable_hz"] > 0 {
					r = m["input_hz"] / m["durable_hz"]
				}
				fmt.Fprintf(w, " SS  %-21s in/dur=%.1f lag=%.1fs cpu=%.2f %s\n", p.key.addr, r, m["data_lag_s"], m["cpu_cores"], p.flag)
			case "log":
				fmt.Fprintf(w, " LOG %-21s input=%s/s q_disk=%s cpu=%.2f\n", p.key.addr, bytesStr(m["input_hz"]), bytesStr(m["queue_disk_bytes"]), m["cpu_cores"])
			default:
				fmt.Fprintf(w, " SL  %-21s cpu=%.2f (%s)\n", p.key.addr, m["cpu_cores"], p.key.role)
			}
		}
	} else {
		groups := map[string][]procStat{}
		for _, p := range procs {
			g := p.key.role
			if g != "storage" && g != "log" {
				g = "other"
			}
			groups[g] = append(groups[g], p)
		}
		rng := func(ps []procStat, col string, f func(float64) string) string {
			lo, hi := math.Inf(1), math.Inf(-1)
			for _, p := range ps {
				v := p.med[col]
				lo, hi = math.Min(lo, v), math.Max(hi, v)
			}
			if lo == hi {
				return f(lo)
			}
			return f(lo) + "-" + f(hi)
		}
		f2 := func(v float64) string { return fmt.Sprintf("%.2f", v) }
		if ps := groups["storage"]; len(ps) > 0 {
			lo, hi := math.Inf(1), math.Inf(-1)
			for _, p := range ps {
				if p.med["durable_hz"] > 0 {
					r := p.med["input_hz"] / p.med["durable_hz"]
					lo, hi = math.Min(lo, r), math.Max(hi, r)
				}
			}
			ratio := "n/a"
			if !math.IsInf(lo, 1) {
				ratio = fmt.Sprintf("%.1f-%.1fx", lo, hi)
			}
			fmt.Fprintf(w, " SS  (%d)   in/dur=%s   lag=%ss   cpu=%s   mem=%s\n", len(ps), ratio,
				rng(ps, "data_lag_s", func(v float64) string { return fmt.Sprintf("%.1f", v) }), rng(ps, "cpu_cores", f2),
				rng(ps, "mem_bytes", bytesStr))
		}
		if ps := groups["log"]; len(ps) > 0 {
			fmt.Fprintf(w, " LOG (%d)   input=%s/s   q_disk=%s   cpu=%s\n", len(ps), rng(ps, "input_hz", bytesStr),
				rng(ps, "queue_disk_bytes", bytesStr), rng(ps, "cpu_cores", f2))
		}
		if ps := groups["other"]; len(ps) > 0 {
			rs := map[string]bool{}
			for _, p := range ps {
				for _, r := range strings.Split(p.key.role, ",") {
					rs[r] = true
				}
			}
			var names []string
			for r := range rs {
				names = append(names, r)
			}
			sort.Strings(names)
			fmt.Fprintf(w, " SL  (%d)   cpu=%s (%s)\n", len(ps), rng(ps, "cpu_cores", f2), strings.Join(names, ", "))
		}
	}

	fmt.Fprintln(w, "\n--- Health ---")
	bad := 0
	for _, p := range procs {
		if p.flag != "" {
			bad++
			fmt.Fprintf(w, " [WARN] storage %s: %s\n", p.key.addr, p.flag)
		}
	}
	stalled := 0
	for _, p := range procs {
		if p.key.role == "storage" && p.med["durable_hz"] == 0 && (p.med["input_hz"] > 0 || p.med["queue_disk_bytes"] > 0) {
			stalled++
			fmt.Fprintf(w, " [WARN] durability stalled on storage %s (durable_hz=0 while data is queued)\n", p.key.addr)
		}
	}
	if bad == 0 && stalled == 0 {
		fmt.Fprintln(w, " [OK] durability kept up with input on all storage servers")
	}
	// TLog queue growth: first vs second half of the analysis window.
	var first, second []float64
	mid := from + (to-from)/2
	for k, cols := range d.fdbProc {
		if k.role != "log" {
			continue
		}
		for _, s := range cols["queue_disk_bytes"] {
			if s.ts < from || s.ts > to {
				continue
			}
			if s.ts <= mid {
				first = append(first, s.v)
			} else {
				second = append(second, s.v)
			}
		}
	}
	if len(first) > 0 && len(second) > 0 {
		a, b := stats.Summarize(first, 10).Avg, stats.Summarize(second, 10).Avg
		if b > a*1.5 && b > 1e6 {
			fmt.Fprintf(w, " [WARN] TLog queue_disk shows sustained growth: first half %s, second half %s\n", bytesStr(a), bytesStr(b))
		} else {
			fmt.Fprintf(w, " [OK] TLog queue stable (first half %s, second half %s)\n", bytesStr(a), bytesStr(b))
		}
	}
	// Ratekeeper limiting queue (~50% of the 1.5GB storage hard limit).
	if v := inSamples(d.fdbCluster["limiting_queue_ss"], from, to); len(v) > 0 {
		if mx := stats.Summarize(v, 10).Max; mx > 750e6 {
			fmt.Fprintf(w, " [WARN] ratekeeper limiting storage queue reached %s\n", bytesStr(mx))
		}
	}
}

func bytesStr(b float64) string {
	switch {
	case b >= 1e9:
		return fmt.Sprintf("%.1fGB", b/1e9)
	case b >= 1e6:
		return fmt.Sprintf("%.0fMB", b/1e6)
	case b >= 1e3:
		return fmt.Sprintf("%.0fKB", b/1e3)
	}
	return fmt.Sprintf("%.0fB", b)
}
