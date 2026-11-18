#!/usr/bin/env python3
"""Run N kv-rgw-backend --perf instances in docker against one FDB cluster.

Replaces scripts/perf_multi.sh, scripts/run_gateway.sh and scripts/.env.

  kvrgw_perf.py run [N] [--pin] [--duration S] [--tarball F] [--image NAME] [--set ...]
  kvrgw_perf.py interactive      one container with a TTY on the driver prompt
  kvrgw_perf.py load             docker-load the image tarball
  kvrgw_perf.py print-config [N] show resolved settings and commands; runs nothing

Settings come from kvrgw_perf.ini (next to this script, or -c FILE). Precedence:
CLI flags / --set > environment variables (PERF_DURATION, PIN, ...) > ini > defaults.
Every instance gets its own tenant <tenant_prefix>-<run-id>-<i>, KVRGW_INSTANCE_ID=i
and its own buckets. The tarball is always docker-loaded before run/interactive.
Output: <logdir>/instance-<i>.log, metrics-<i>.csv, live.csv.
"""
import argparse
import configparser
import datetime
import gzip
import os
import re
import shlex
import shutil
import signal
import subprocess
import sys
import threading
import time

HERE = os.path.dirname(os.path.abspath(__file__))
DEFAULT_CONFIG = os.path.join(HERE, "kvrgw_perf.ini")

# (section, key, environment variable, default)
SETTINGS = [
    ("run", "instances", None, "3"),
    ("run", "tenant_prefix", "PERF_TENANT_PREFIX", "perf"),
    ("run", "pin", "PIN", "no"),
    ("run", "cpu_list", "CPU_LIST", ""),
    ("run", "live", "LIVE", "yes"),
    ("run", "live_interval", "LIVE_INTERVAL", "2"),
    ("run", "metrics_interval_ms", "KVRGW_METRICS_INTERVAL_MS", "1000"),
    ("run", "logdir", "LOGDIR", ""),
    ("run", "run_id", "RUN_ID", ""),
    ("docker", "docker", "DOCKER", "docker"),
    ("docker", "image", "IMAGE", "kvrgw-gw"),
    ("docker", "tarball", "TARBALL", "kvrgw-gw.tar.gz"),
    ("docker", "run_args", "DOCKER_RUN_ARGS", "--security-opt seccomp=unconfined"),
    ("docker", "cluster_file", "CLUSTER_FILE", "/etc/foundationdb/fdb.cluster"),
    ("workload", "buckets", "PERF_BUCKETS", "4"),
    ("workload", "mode", "PERF_MODE", "versioned"),
    ("workload", "c", "PERF_C", "128"),
    ("workload", "tiers", "PERF_TIERS", "8192"),
    ("workload", "duration", "PERF_DURATION", "180"),
    ("workload", "burst", "PERF_BURST", "5"),
    ("workload", "sim_disk_write_us", "PERF_SIM_DISK_WRITE_US", "0"),
    ("workload", "sim_disk_read_us", "PERF_SIM_DISK_READ_US", "0"),
    ("workload", "cleanup", "PERF_CLEANUP", "no"),
    ("workload", "extra_commands", None, ""),
]

# [backend] key -> container environment variable (also read from the environment).
BACKEND = {
    "max_inline": "KVRGW_MAX_INLINE",
    "max_kv_store": "KVRGW_MAX_KV_STORE",
    "batch_size": "KVRGW_BATCH_SIZE",
    "batch_timeout_us": "KVRGW_BATCH_TIMEOUT_US",
    "batch_threads": "KVRGW_BATCH_THREADS",
    "prefix_len": "KVRGW_PREFIX_LEN",
    "suffix_len": "KVRGW_SUFFIX_LEN",
    "tag_count": "KVRGW_TAG_COUNT",
    "tag_name_base": "KVRGW_TAG_NAME_BASE",
    "tag_data_size": "KVRGW_TAG_DATA_SIZE",
    "batch_max_inflight": "KVRGW_BATCH_MAX_INFLIGHT",
    "kv_store_coalescing": "KVRGW_KV_STORE_COALESCING",
    "gomaxprocs": "KVRGW_GOMAXPROCS",
    "debug": "KVRGW_DEBUG",
    "key_prefix": "KVRGW_KEY_PREFIX",
    "key_suffix": "KVRGW_KEY_SUFFIX",
    "random_prefix_per_key": "KVRGW_RANDOM_PREFIX_PER_KEY",
    "metadata_count": "KVRGW_METADATA_COUNT",
    "metadata_data_size": "KVRGW_METADATA_DATA_SIZE",
    "root_access_key": "ROOT_ACCESS_KEY",
    "root_secret_key": "ROOT_SECRET_KEY",
    "aws_default_region": "AWS_DEFAULT_REGION",
    "tenant_name": "KVRGW_TENANT_NAME",
}
BACKEND_DEFAULTS = {
    "max_inline": "256", "max_kv_store": "4096", "batch_size": "10",
    "batch_timeout_us": "1000", "batch_threads": "8", "prefix_len": "16",
    "suffix_len": "8", "tag_count": "2", "tag_name_base": "mytag",
    "tag_data_size": "200",
}

ENTRYPOINT = "/opt/kvrgw/kv-rgw-backend"
PERF_ARGS = ["--perf", "/tmp/kvrgw-perf.sock", "/tmp/perf-data"]
RESULT_RE = re.compile(r"ops=(\d+)\s+errors=(\d+)\s+retries=(\d+)\s+IOPS=(\d+)")


class ConfigError(Exception):
    pass


def as_bool(value, name):
    v = value.strip().lower()
    if v in ("1", "yes", "true", "on"):
        return True
    if v in ("0", "no", "false", "off", ""):
        return False
    raise ConfigError(f"{name}: expected yes/no, got {value!r}")


def as_int(value, name, minimum=0):
    try:
        n = int(value)
    except ValueError:
        raise ConfigError(f"{name}: expected an integer, got {value!r}") from None
    if n < minimum:
        raise ConfigError(f"{name}: must be >= {minimum}, got {n}")
    return n


def load_config(path, overrides):
    """Return {section: {key: str}} merged from defaults, ini, environment and overrides."""
    parser = configparser.ConfigParser(interpolation=None)
    if path:
        if not parser.read(path):
            raise ConfigError(f"cannot read config file {path}")
    cfg = {"run": {}, "docker": {}, "backend": {}, "workload": {}}

    def from_file(section, key):
        return parser.get(section, key, fallback=None)

    for section, key, env, default in SETTINGS:
        val = from_file(section, key)
        if env and env in os.environ:
            val = os.environ[env]
        cfg[section][key] = default if val is None else val
    for key, env in BACKEND.items():
        val = from_file("backend", key)
        if env in os.environ:
            val = os.environ[env]
        cfg["backend"][key] = BACKEND_DEFAULTS.get(key, "") if val is None else val

    for item in overrides:
        target, sep, val = item.partition("=")
        section, dot, key = target.partition(".")
        if not sep or not dot or section not in cfg or key not in cfg[section]:
            raise ConfigError(f"--set {item!r}: expected section.key=value with a known key")
        cfg[section][key] = val
    return cfg


def validate(cfg):
    as_int(cfg["run"]["instances"], "run.instances", 1)
    as_int(cfg["run"]["live_interval"], "run.live_interval", 1)
    as_int(cfg["run"]["metrics_interval_ms"], "run.metrics_interval_ms", 0)
    as_bool(cfg["run"]["pin"], "run.pin")
    as_bool(cfg["run"]["live"], "run.live")
    as_bool(cfg["workload"]["cleanup"], "workload.cleanup")
    for key in ("buckets", "c", "tiers", "duration", "burst",
                "sim_disk_write_us", "sim_disk_read_us"):
        as_int(cfg["workload"][key], f"workload.{key}", 0)


def tarball_path(cfg):
    return os.path.join(HERE, cfg["docker"]["tarball"])


def docker_load(cfg):
    path = tarball_path(cfg)
    if not os.path.isfile(path):
        raise ConfigError(f"image tarball not found: {path}")
    print(f"loading {path}", flush=True)
    proc = subprocess.Popen([cfg["docker"]["docker"], "load"], stdin=subprocess.PIPE,
                            stdout=subprocess.DEVNULL)
    try:
        with gzip.open(path, "rb") as src:
            shutil.copyfileobj(src, proc.stdin)
    except BrokenPipeError:
        pass
    finally:
        try:
            proc.stdin.close()
        except BrokenPipeError:
            pass
    if proc.wait() != 0:
        raise ConfigError("docker load failed")


def driver_commands(cfg):
    w, b = cfg["workload"], cfg["backend"]
    cmds = [f"create-buckets buckets={w['buckets']} mode={w['mode']}"]
    if b["batch_size"] and b["batch_timeout_us"] and b["batch_threads"]:
        cmds.append(f"set-batch size={b['batch_size']} timeout={b['batch_timeout_us']} "
                    f"threads={b['batch_threads']}")
    cmds.append(f"set-sim-disk-write-us {w['sim_disk_write_us']}")
    cmds.append(f"set-sim-disk-read-us {w['sim_disk_read_us']}")
    cmds.append(f"put c={w['c']} tiers={w['tiers']} duration={w['duration']} "
                f"burst={w['burst']}")
    cmds += [ln.strip() for ln in w["extra_commands"].splitlines() if ln.strip()]
    if as_bool(w["cleanup"], "workload.cleanup"):
        cmds += [f"delete c={w['c']}", "delete-buckets"]
    cmds.append("quit")
    return cmds


def online_cpus(cpu_list):
    if not cpu_list:
        with open("/sys/devices/system/cpu/online") as f:
            cpu_list = f.read().strip()
    cpus = []
    for part in cpu_list.split(","):
        part = part.strip()
        if "-" in part:
            lo, hi = part.split("-")
            cpus += range(int(lo), int(hi) + 1)
        elif part:
            cpus.append(int(part))
    return cpus


def cpuset_for(i, n, cpus):
    """Even contiguous slice of cpus for instance i, or one shared core if n > cores."""
    m = len(cpus)
    if n <= m:
        lo, hi = i * m // n, (i + 1) * m // n
    else:
        lo = i % m
        hi = lo + 1
    return ",".join(str(c) for c in cpus[lo:hi])


def instance_env(cfg, extra):
    env = {}
    for key, name in BACKEND.items():
        if cfg["backend"][key]:
            env[name] = cfg["backend"][key]
    env["KVRGW_METRICS_INTERVAL_MS"] = cfg["run"]["metrics_interval_ms"]
    env.update({k: v for k, v in extra.items() if v})
    return env


def docker_run_cmd(cfg, name, env, cpuset="", metrics_dir="", tty=False):
    d = cfg["docker"]
    cmd = [d["docker"], "run", "--rm", "-it" if tty else "-i", "--init"]
    cmd += shlex.split(d["run_args"])
    cmd += ["--network", "host", "--name", name]
    if cpuset:
        cmd += ["--cpuset-cpus", cpuset]
    cmd += ["-v", f"{d['cluster_file']}:/etc/foundationdb/fdb.cluster:ro"]
    if metrics_dir:
        cmd += ["-v", f"{metrics_dir}:/metrics"]
    for k, v in env.items():
        cmd += ["-e", f"{k}={v}"]
    cmd += ["--entrypoint", ENTRYPOINT, d["image"]] + PERF_ARGS
    return cmd


def check_cluster_file(cfg):
    if not os.path.isfile(cfg["docker"]["cluster_file"]):
        raise ConfigError(f"no FDB cluster file at {cfg['docker']['cluster_file']}")


class Plan:
    """Everything needed to launch the instances, derived from the config."""

    def __init__(self, cfg, n):
        self.cfg, self.n = cfg, n
        r = cfg["run"]
        self.run_id = r["run_id"] or datetime.datetime.now().strftime("%Y%m%d%H%M%S")
        logdir = r["logdir"] or os.path.join(os.getcwd(), "perf-logs", self.run_id)
        self.logdir = os.path.abspath(logdir)
        self.metrics_ms = as_int(r["metrics_interval_ms"], "run.metrics_interval_ms")
        self.live_interval = as_int(r["live_interval"], "run.live_interval", 1)
        self.live = as_bool(r["live"], "run.live") and self.metrics_ms > 0
        self.cpus = online_cpus(r["cpu_list"]) if as_bool(r["pin"], "run.pin") else []
        self.commands = driver_commands(cfg)

    def name(self, i):
        return f"kvrgw-perf-{self.run_id}-{i}"

    def tenant(self, i):
        return f"{self.cfg['run']['tenant_prefix']}-{self.run_id}-{i}"

    def cmd(self, i):
        extra = {"KVRGW_INSTANCE_ID": str(i), "KVRGW_TENANT_NAME": self.tenant(i)}
        if self.metrics_ms > 0:
            extra["KVRGW_METRICS_FILE"] = f"/metrics/metrics-{i}.csv"
        cpuset = cpuset_for(i, self.n, self.cpus) if self.cpus else ""
        return docker_run_cmd(self.cfg, self.name(i), instance_env(self.cfg, extra), cpuset,
                              self.logdir if self.metrics_ms > 0 else "")

    def cpuset(self, i):
        return cpuset_for(i, self.n, self.cpus) if self.cpus else ""


def live_sample(plan):
    """Aggregate rate over all instances: (total, reporting, min, max, errors)."""
    now_ms = int(time.time() * 1000)
    stale = plan.live_interval * 3000 + plan.metrics_ms
    rates, errors = [], 0
    for i in range(plan.n):
        path = os.path.join(plan.logdir, f"metrics-{i}.csv")
        try:
            with open(path) as f:
                lines = f.readlines()[-64:]
        except OSError:
            continue
        t = pt = None
        ops, prev_ops, err = {}, {}, 0
        for line in lines:
            fld = line.strip().split(",")
            if len(fld) < 5 or not fld[0].isdigit():
                continue
            ts = int(fld[0])
            if ts != t:
                pt, prev_ops, t = t, ops, ts
                ops, err = {}, 0
            try:
                ops[fld[2]] = float(fld[3])
                err += int(float(fld[4]))
            except ValueError:
                continue
        if pt is None or t is None or now_ms - t > stale:
            continue
        delta = sum(ops.values()) - sum(prev_ops.values())
        if delta < 0 or t - pt <= 0:
            errors += err  # counters were reset between commands
            continue
        rates.append(delta * 1000.0 / (t - pt))
        errors += err
    if not rates:
        return 0, 0, 0, 0, errors
    return sum(rates), len(rates), min(rates), max(rates), errors


def live_loop(plan, stop):
    start = time.monotonic()
    live_csv = os.path.join(plan.logdir, "live.csv")
    with open(live_csv, "w") as out:
        out.write("elapsed,total_ops_s,reporting,min_ops_s,max_ops_s,errors\n")
        while not stop.wait(plan.live_interval):
            el = int(time.monotonic() - start)
            tot, rep, mn, mx, err = live_sample(plan)
            stamp = f"[{el // 60:02d}:{el % 60:02d}]"
            if rep == 0:
                print(f"{stamp} starting... (0/{plan.n} reporting)", flush=True)
                continue
            print(f"{stamp} total {tot:.0f} ops/s | {rep}/{plan.n} instances | "
                  f"per-instance min {mn:.0f} max {mx:.0f} | errors {err}", flush=True)
            out.write(f"{el},{tot:.0f},{rep},{mn:.0f},{mx:.0f},{err}\n")
            out.flush()


def summarize(plan, rcs):
    print(f"\n{'instance':<9} {'rc':<4} {'ops':>12} {'errors':>8} {'retries':>8} {'IOPS':>10}")
    tot_ops = tot_err = tot_iops = 0
    for i in range(plan.n):
        result = None
        in_put = False
        with open(os.path.join(plan.logdir, f"instance-{i}.log"), errors="replace") as f:
            for line in f:
                if line.startswith("=== PUT"):
                    in_put = True
                    continue
                m = RESULT_RE.search(line) if in_put else None
                if m:
                    result = m
                    break
        if result:
            ops, err, ret, iops = (int(x) for x in result.groups())
            tot_ops, tot_err, tot_iops = tot_ops + ops, tot_err + err, tot_iops + iops
            print(f"{i:<9} {rcs[i]:<4} {ops:>12} {err:>8} {ret:>8} {iops:>10}")
        else:
            print(f"{i:<9} {rcs[i]:<4} no PUT result (see "
                  f"{os.path.join(plan.logdir, f'instance-{i}.log')})")
    print(f"{'total':<9} {'':<4} {tot_ops:>12} {tot_err:>8} {'':>8} {tot_iops:>10}")
    rates = []
    try:
        with open(os.path.join(plan.logdir, "live.csv")) as f:
            next(f)
            rates = [float(line.split(",")[1]) for line in f if line.count(",") >= 5]
    except (OSError, StopIteration, ValueError):
        pass
    if rates:
        print(f"live total ops/s: peak {max(rates):.0f}, average "
              f"{sum(rates) / len(rates):.0f} ({len(rates)} samples)")
    print(f"logs: {plan.logdir}")


def cmd_run(cfg, n):
    check_cluster_file(cfg)
    docker_load(cfg)
    plan = Plan(cfg, n)
    os.makedirs(plan.logdir, exist_ok=True)
    stdin_data = ("\n".join(plan.commands) + "\n").encode()

    procs, logs = [], []
    stop = threading.Event()
    sampler = None

    def cleanup(signum=None, frame=None):
        print("interrupted; stopping instances", file=sys.stderr)
        stop.set()
        for i in range(plan.n):
            subprocess.run([cfg["docker"]["docker"], "rm", "-f", plan.name(i)],
                           stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        for p in procs:
            p.terminate()
        sys.exit(130)

    signal.signal(signal.SIGINT, cleanup)
    signal.signal(signal.SIGTERM, cleanup)

    print(f"run {plan.run_id}: {plan.n} instance(s), logs in {plan.logdir}")
    for i in range(plan.n):
        if plan.metrics_ms > 0:
            open(os.path.join(plan.logdir, f"metrics-{i}.csv"), "w").close()
        log = open(os.path.join(plan.logdir, f"instance-{i}.log"), "w")
        logs.append(log)
        p = subprocess.Popen(plan.cmd(i), stdin=subprocess.PIPE, stdout=log,
                             stderr=subprocess.STDOUT)
        p.stdin.write(stdin_data)
        p.stdin.close()
        procs.append(p)
        cpus = plan.cpuset(i)
        print(f"  instance {i}: tenant {plan.tenant(i)}" + (f", cpus {cpus}" if cpus else ""))

    if plan.live:
        sampler = threading.Thread(target=live_loop, args=(plan, stop), daemon=True)
        sampler.start()
    rcs = [p.wait() for p in procs]
    stop.set()
    if sampler:
        sampler.join()
    for log in logs:
        log.close()
    summarize(plan, rcs)
    return 1 if any(rcs) else 0


def cmd_interactive(cfg):
    check_cluster_file(cfg)
    docker_load(cfg)
    run_id = cfg["run"]["run_id"] or datetime.datetime.now().strftime("%Y%m%d%H%M%S")
    env = instance_env(cfg, {})
    cmd = docker_run_cmd(cfg, f"kvrgw-perf-{run_id}-interactive", env, tty=True)
    return subprocess.call(cmd)


def cmd_print_config(cfg, n):
    plan = Plan(cfg, n)
    for section, items in cfg.items():
        print(f"[{section}]")
        for key, val in items.items():
            print(f"  {key} = {val}")
    print(f"\nrun id {plan.run_id}, logdir {plan.logdir}, instances {plan.n}")
    print("\ndriver commands (per instance):")
    for c in plan.commands:
        print(f"  {c}")
    for i in sorted({0, plan.n - 1}):
        print(f"\ninstance {i}:")
        print("  " + shlex.join(plan.cmd(i)))
    return 0


def build_parser():
    p = argparse.ArgumentParser(prog="kvrgw_perf.py", description=__doc__.split("\n")[0])
    common = argparse.ArgumentParser(add_help=False)
    common.add_argument("-c", "--config", default=argparse.SUPPRESS,
                        help="config file (default: kvrgw_perf.ini next to the script)")
    common.add_argument("--set", action="append", default=argparse.SUPPRESS,
                        metavar="SECTION.KEY=VALUE", help="override a config value (repeatable)")
    sub = p.add_subparsers(dest="command", required=True)
    for name, helptext in (("run", "run N parallel --perf instances"),
                           ("print-config", "show resolved settings and commands; runs nothing")):
        sp = sub.add_parser(name, help=helptext, parents=[common])
        sp.add_argument("n", nargs="?", type=int, help="number of instances")
        sp.add_argument("--pin", action="store_true", help="pin each instance to its own cores")
        sp.add_argument("--cpu-list", help="cores to spread over, e.g. 0-7,16-23")
        sp.add_argument("--tarball", help="image tarball to docker-load (relative to this script)")
        sp.add_argument("--image", help="image name:tag to run, if the tarball is tagged differently")
        sp.add_argument("--duration", help="put duration in seconds")
        sp.add_argument("--run-id")
        sp.add_argument("--logdir")
        sp.add_argument("--no-live", action="store_true", help="disable the live throughput line")
    for name, helptext in (("interactive", "one container with a TTY on the driver prompt"),
                           ("load", "docker-load the image tarball")):
        sp = sub.add_parser(name, parents=[common], help=helptext)
        sp.add_argument("--tarball", help="image tarball to docker-load")
        sp.add_argument("--image", help="image name:tag to run")
    return p


def main():
    args = build_parser().parse_args()
    overrides = list(getattr(args, "set", []))
    for flag, target in (("cpu_list", "run.cpu_list"), ("duration", "workload.duration"),
                         ("run_id", "run.run_id"), ("logdir", "run.logdir"),
                         ("tarball", "docker.tarball"), ("image", "docker.image")):
        if getattr(args, flag, None) is not None:
            overrides.append(f"{target}={getattr(args, flag)}")
    if getattr(args, "pin", False):
        overrides.append("run.pin=yes")
    if getattr(args, "no_live", False):
        overrides.append("run.live=no")
    if getattr(args, "n", None) is not None:
        overrides.append(f"run.instances={args.n}")

    try:
        path = getattr(args, "config", None) or (DEFAULT_CONFIG if os.path.isfile(DEFAULT_CONFIG) else None)
        cfg = load_config(path, overrides)
        validate(cfg)
        n = as_int(cfg["run"]["instances"], "run.instances", 1)
        if args.command == "run":
            return cmd_run(cfg, n)
        if args.command == "interactive":
            return cmd_interactive(cfg)
        if args.command == "load":
            docker_load(cfg)
            return 0
        return cmd_print_config(cfg, n)
    except ConfigError as e:
        print(f"error: {e}", file=sys.stderr)
        return 2


if __name__ == "__main__":
    sys.exit(main())
