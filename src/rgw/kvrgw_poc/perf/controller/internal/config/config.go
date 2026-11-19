// Package config loads the perf harness config (base <- command section <- CLI flags).
package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// Commands lists the valid operation commands (report is handled separately).
var Commands = []string{
	"load", "run-put-object", "run-list-objects", "run-put-object-overwrite",
	"run-put-object-overwrite-versioned", "run-delete-multi",
}

func IsCommand(c string) bool {
	for _, x := range Commands {
		if x == c {
			return true
		}
	}
	return false
}

// Config is the fully resolved configuration. yaml keys == CLI flag names (snake_case).
type Config struct {
	NumObjects       uint64 `yaml:"num_objects" json:"num_objects"`
	NumBuckets       uint64 `yaml:"num_buckets" json:"num_buckets"`
	BucketNamePrefix string `yaml:"bucket_name_prefix" json:"bucket_name_prefix"`
	PaddingBytes     int    `yaml:"padding_bytes" json:"padding_bytes"`
	ObjectSizeBytes  uint64 `yaml:"object_size_bytes" json:"object_size_bytes"`
	WorkerCount      int    `yaml:"worker_count" json:"worker_count"`
	ThreadsPerWorker int    `yaml:"threads_per_worker" json:"threads_per_worker"`
	VerifyThreads    int    `yaml:"verify_threads" json:"verify_threads"`
	TenantName       string `yaml:"tenant_name" json:"tenant_name"`
	BatchSize        *int   `yaml:"batch_size" json:"batch_size,omitempty"`
	BatchTimeoutUs   *int   `yaml:"batch_timeout_us" json:"batch_timeout_us,omitempty"`
	BatchThreads     *int   `yaml:"batch_threads" json:"batch_threads,omitempty"`
	ReportIntervalMs int64  `yaml:"report_interval_ms" json:"report_interval_ms"`

	FdbStats            bool   `yaml:"fdb_stats" json:"fdb_stats"`
	FdbClusterFile      string `yaml:"fdb_cluster_file" json:"fdb_cluster_file"`
	FdbcliPath          string `yaml:"fdbcli_path" json:"fdbcli_path"`
	FdbSampleIntervalMs int64  `yaml:"fdb_sample_interval_ms" json:"fdb_sample_interval_ms"`
	CpuSampleIntervalMs int64  `yaml:"cpu_sample_interval_ms" json:"cpu_sample_interval_ms"`

	// run-* only
	Ops      uint64 `yaml:"ops" json:"ops,omitempty"`
	Duration string `yaml:"duration" json:"duration,omitempty"`

	PageSize              int    `yaml:"page_size" json:"page_size"`
	SingleThreadMode      bool   `yaml:"single_thread_mode" json:"single_thread_mode"`
	NumObjectsToOverwrite uint64 `yaml:"num_objects_to_overwrite" json:"num_objects_to_overwrite"`
}

// Defaults returns built-in defaults (batch_* unset => the runtime's kvrgw.yaml values).
func Defaults() Config {
	return Config{
		NumObjects: 1000000, NumBuckets: 10, BucketNamePrefix: "rgwkv",
		PaddingBytes: 16, ObjectSizeBytes: 8192, WorkerCount: 1, ThreadsPerWorker: 1,
		VerifyThreads: 4, TenantName: envOr("KVRGW_TENANT_NAME", "kv-poc"),
		ReportIntervalMs: 1000,
		FdbStats:         true, FdbClusterFile: os.Getenv("FDB_CLUSTER_FILE"), FdbcliPath: defaultFdbcli(),
		FdbSampleIntervalMs: 2000, CpuSampleIntervalMs: 1000,
		PageSize: 1000,
	}
}

// defaultFdbcli is the container's fdbcli location when present, else fdbcli on PATH.
func defaultFdbcli() string {
	const p = "/opt/foundationdb/usr/bin/fdbcli"
	if _, err := os.Stat(p); err == nil {
		return p
	}
	return "fdbcli"
}

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

// sectionNames are the allowed top-level keys of the file.
var sectionNames = append([]string{"base"}, Commands...)

// File is the parsed config file: raw per-section YAML.
type File struct {
	sections map[string][]byte
}

// LoadFile parses YAML/JSON; unknown section names and unknown keys are errors.
func LoadFile(path string) (*File, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ParseFile(data)
}

func ParseFile(data []byte) (*File, error) {
	var raw map[string]yaml.Node
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	f := &File{sections: map[string][]byte{}}
	for k, n := range raw {
		ok := false
		for _, s := range sectionNames {
			if s == k {
				ok = true
			}
		}
		if !ok {
			return nil, fmt.Errorf("config: unknown section %q (valid: base, %v)", k, Commands)
		}
		if n.Kind == yaml.ScalarNode && n.Tag == "!!null" {
			continue
		}
		b, err := yaml.Marshal(&n)
		if err != nil {
			return nil, err
		}
		f.sections[k] = b
	}
	return f, nil
}

func decodeStrict(b []byte, into *Config) error {
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(into); err != nil && err.Error() != "EOF" {
		return err
	}
	return nil
}

// Resolve = defaults <- base <- command section. (CLI overrides are applied by the caller
// with Set.) f may be nil.
func Resolve(f *File, command string) (Config, error) {
	cfg := Defaults()
	if f != nil {
		for _, sec := range []string{"base", command} {
			if b, ok := f.sections[sec]; ok {
				if err := decodeStrict(b, &cfg); err != nil {
					return cfg, fmt.Errorf("config section %q: %w", sec, err)
				}
			}
		}
	}
	return cfg, nil
}

// DurationValue parses cfg.Duration (Go duration syntax); 0 if unset.
func (c *Config) DurationValue() (time.Duration, error) {
	if c.Duration == "" {
		return 0, nil
	}
	return time.ParseDuration(c.Duration)
}

// Validate checks everything that can be checked before workers are started.
func (c *Config) Validate(command string) error {
	if c.NumObjects == 0 {
		return fmt.Errorf("num_objects must be > 0")
	}
	if c.NumBuckets == 0 {
		return fmt.Errorf("num_buckets must be > 0")
	}
	if c.WorkerCount < 1 || c.ThreadsPerWorker < 1 {
		return fmt.Errorf("worker_count and threads_per_worker must be >= 1")
	}
	if c.WorkerCount*c.ThreadsPerWorker > 65536 {
		return fmt.Errorf("worker_count * threads_per_worker must be <= 65536")
	}
	if c.VerifyThreads < 1 {
		return fmt.Errorf("verify_threads must be >= 1")
	}
	if c.PaddingBytes < 0 || c.PaddingBytes > 700 {
		return fmt.Errorf("padding_bytes out of range")
	}
	if c.PageSize < 1 {
		return fmt.Errorf("page_size must be >= 1")
	}
	if c.ReportIntervalMs < 0 {
		return fmt.Errorf("report_interval_ms must be >= 0")
	}
	d, err := c.DurationValue()
	if err != nil {
		return fmt.Errorf("duration: %w", err)
	}
	if d < 0 {
		return fmt.Errorf("duration must be positive")
	}
	if command == "load" {
		if c.Ops != 0 || d != 0 {
			return fmt.Errorf("ops/duration are not valid for load")
		}
	} else {
		if (c.Ops != 0) == (d != 0) {
			return fmt.Errorf("%s requires exactly one of --ops or --duration", command)
		}
	}
	if command == "run-put-object-overwrite" || command == "run-put-object-overwrite-versioned" {
		if c.NumObjectsToOverwrite == 0 {
			return fmt.Errorf("%s requires num_objects_to_overwrite > 0", command)
		}
	}
	return nil
}

// ApplyCommandRules adjusts the config for command-specific semantics. It returns
// human-readable warnings.
func (c *Config) ApplyCommandRules(command string) (warnings []string) {
	switch command {
	case "run-list-objects":
		if c.SingleThreadMode {
			c.WorkerCount, c.ThreadsPerWorker = 1, 1
		} else if uint64(c.WorkerCount) > c.NumBuckets {
			warnings = append(warnings, fmt.Sprintf(
				"num_buckets (%d) < worker_count (%d): capping worker_count at %d so every worker owns a bucket",
				c.NumBuckets, c.WorkerCount, c.NumBuckets))
			c.WorkerCount = int(c.NumBuckets)
		}
	case "run-delete-multi":
		if c.SingleThreadMode {
			c.WorkerCount, c.ThreadsPerWorker = 1, 1
		}
	}
	return
}

// workerJSON is what workers receive (no collector keys).
type workerJSON struct {
	NumObjects            uint64 `json:"num_objects"`
	NumBuckets            uint64 `json:"num_buckets"`
	BucketNamePrefix      string `json:"bucket_name_prefix"`
	PaddingBytes          int    `json:"padding_bytes"`
	ObjectSizeBytes       uint64 `json:"object_size_bytes"`
	ThreadsPerWorker      int    `json:"threads_per_worker"`
	VerifyThreads         int    `json:"verify_threads"`
	TenantName            string `json:"tenant_name"`
	BatchSize             *int   `json:"batch_size,omitempty"`
	BatchTimeoutUs        *int   `json:"batch_timeout_us,omitempty"`
	BatchThreads          *int   `json:"batch_threads,omitempty"`
	ReportIntervalMs      int64  `json:"report_interval_ms"`
	Ops                   uint64 `json:"ops,omitempty"`
	DurationMs            int64  `json:"duration_ms,omitempty"`
	PageSize              int    `json:"page_size"`
	SingleThreadMode      bool   `json:"single_thread_mode"`
	NumObjectsToOverwrite uint64 `json:"num_objects_to_overwrite"`
}

// WorkerJSON renders the config sent in InitRequest.config_json.
func (c *Config) WorkerJSON() ([]byte, error) {
	d, err := c.DurationValue()
	if err != nil {
		return nil, err
	}
	return json.Marshal(workerJSON{
		c.NumObjects, c.NumBuckets, c.BucketNamePrefix, c.PaddingBytes, c.ObjectSizeBytes,
		c.ThreadsPerWorker, c.VerifyThreads, c.TenantName, c.BatchSize, c.BatchTimeoutUs,
		c.BatchThreads, c.ReportIntervalMs, c.Ops, d.Milliseconds(), c.PageSize,
		c.SingleThreadMode, c.NumObjectsToOverwrite,
	})
}

// ResolvedJSON is the pretty full config written to config.resolved.json.
func (c *Config) ResolvedJSON() ([]byte, error) { return json.MarshalIndent(c, "", "  ") }
