// kvrgw-perf: controller for the kvrgw perf harness.
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"kvrgw-perf/internal/config"
	"kvrgw-perf/internal/controller"
	"kvrgw-perf/internal/local"
	"kvrgw-perf/internal/report"
)

const usage = `usage: kvrgw-perf [global flags] <command> [flags]

commands:
  load                                 create buckets and load objects
  run-put-object                       put brand-new keys
  run-list-objects                     page through listings
  run-put-object-overwrite             overwrite existing keys
  run-put-object-overwrite-versioned   overwrite and verify version counts
  run-delete-multi                     batch deletes (destructive)
  report <results-dir>                 analyse a results directory
        [--warmup D] [--deviation N] [--per-server]

global flags:
  --config FILE          harness config (default ./kvrgw-perf.yaml if present)
  --workers N            worker_count          --threads M   threads_per_worker
  --worker-binary PATH   kvrgw-perf-worker     --base-port P (default 9090)
  --cpus LIST            CPU pool to pin workers to, e.g. 0-15
  --run-dir DIR          results directory (default perf-results/<UTC ts>-<command>)
  --yes                  skip confirmation prompts
  --no-verify            skip the Verify step
  --no-fdb-stats         do not sample FDB status
  --fdb-cluster-file P   cluster file for fdbcli
  any config key as --kebab-case (e.g. --num-objects, --page-size, --ops, --duration 30s)
`

type globals struct {
	config, workerBinary, cpus, runDir string
	basePort                           int
	yes, noVerify                      bool
}

func registerGlobals(fs *flag.FlagSet, g *globals, ov *config.Overrides) {
	fs.StringVar(&g.config, "config", g.config, "")
	fs.StringVar(&g.workerBinary, "worker-binary", g.workerBinary, "")
	fs.StringVar(&g.cpus, "cpus", g.cpus, "")
	fs.StringVar(&g.runDir, "run-dir", g.runDir, "")
	fs.IntVar(&g.basePort, "base-port", g.basePort, "")
	fs.BoolVar(&g.yes, "yes", g.yes, "")
	fs.BoolVar(&g.noVerify, "no-verify", g.noVerify, "")
	ov.Register(fs)
	fs.Usage = func() { fmt.Fprint(os.Stderr, usage) }
}

func main() { os.Exit(run(os.Args[1:])) }

func run(args []string) int {
	g := &globals{basePort: 9090}
	ov := config.NewOverrides()
	fs := flag.NewFlagSet("kvrgw-perf", flag.ContinueOnError)
	registerGlobals(fs, g, ov)
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}
	rest := fs.Args()
	if len(rest) == 0 {
		fmt.Fprint(os.Stderr, usage)
		return 2
	}
	command := rest[0]
	if command == "report" {
		return runReport(rest[1:])
	}
	if !config.IsCommand(command) {
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", command, usage)
		return 2
	}
	// Flags may also follow the command.
	cfs := flag.NewFlagSet(command, flag.ContinueOnError)
	registerGlobals(cfs, g, ov)
	if err := cfs.Parse(rest[1:]); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}
	if cfs.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "unexpected argument %q\n", cfs.Arg(0))
		return 2
	}

	// ---- resolve config ----
	var file *config.File
	cfgPath := g.config
	if cfgPath == "" {
		if _, err := os.Stat("kvrgw-perf.yaml"); err == nil {
			cfgPath = "kvrgw-perf.yaml"
		}
	}
	if cfgPath != "" {
		var err error
		if file, err = config.LoadFile(cfgPath); err != nil {
			fmt.Fprintln(os.Stderr, "config:", err)
			return 2
		}
	}
	cfg, err := config.Resolve(file, command)
	if err != nil {
		fmt.Fprintln(os.Stderr, "config:", err)
		return 2
	}
	if err := ov.Apply(&cfg); err != nil {
		fmt.Fprintln(os.Stderr, "config:", err)
		return 2
	}
	if err := cfg.Validate(command); err != nil {
		fmt.Fprintln(os.Stderr, "config:", err)
		return 2
	}
	warnings := cfg.ApplyCommandRules(command)

	if command == "run-delete-multi" && !g.yes {
		fmt.Print("run-delete-multi deletes objects created by load and cannot be undone. Continue? [y/N] ")
		ans, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		if a := strings.ToLower(strings.TrimSpace(ans)); a != "y" && a != "yes" {
			fmt.Fprintln(os.Stderr, "aborted")
			return 1
		}
	}

	bin := g.workerBinary
	if bin == "" {
		bin = findWorker()
	}
	runDir := g.runDir
	if runDir == "" {
		// resolved here so the launcher and the controller agree on the directory
		runDir = filepath.Join("perf-results", time.Now().UTC().Format("20060102T150405Z")+"-"+command)
	}
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, "run dir:", err)
		return 1
	}
	absRun, _ := filepath.Abs(runDir)
	launcher := &local.Local{Binary: bin, BasePort: g.basePort, RunDir: absRun, CPUs: g.cpus}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return controller.Run(ctx, controller.Options{Command: command, Cfg: cfg, Warnings: warnings,
		RunDir: absRun, NoVerify: g.noVerify, Launcher: launcher})
}

// findWorker prefers a kvrgw-perf-worker next to this executable, else PATH.
func findWorker() string {
	if exe, err := os.Executable(); err == nil {
		p := filepath.Join(filepath.Dir(exe), "kvrgw-perf-worker")
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return "kvrgw-perf-worker"
}

func runReport(args []string) int {
	var o report.Options
	fs := flag.NewFlagSet("report", flag.ContinueOnError)
	warmup := fs.String("warmup", "0s", "duration of the benchmark start to ignore")
	fs.Float64Var(&o.Deviation, "deviation", 10, "percent deviation from the median counted by dev%")
	fs.BoolVar(&o.PerServer, "per-server", false, "one FDB line per process")
	// Accept flags before or after the results directory.
	var dir string
	for len(args) > 0 {
		if err := fs.Parse(args); err != nil {
			return 2
		}
		args = fs.Args()
		if len(args) > 0 {
			if dir != "" {
				fmt.Fprintln(os.Stderr, "report takes exactly one results directory")
				return 2
			}
			dir, args = args[0], args[1:]
		}
	}
	if dir == "" {
		fmt.Fprintln(os.Stderr, "usage: kvrgw-perf report <results-dir> [--warmup D] [--deviation N] [--per-server]")
		return 2
	}
	d, err := time.ParseDuration(*warmup)
	if err != nil || d < 0 {
		fmt.Fprintf(os.Stderr, "invalid --warmup %q\n", *warmup)
		return 2
	}
	o.Warmup = d
	return report.Run(os.Stdout, dir, o)
}
