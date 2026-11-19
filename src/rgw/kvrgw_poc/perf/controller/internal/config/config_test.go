package config

import "testing"

const sample = `
base:
  num_objects: 5000
  num_buckets: 4
  worker_count: 2
  threads_per_worker: 8
load:
  threads_per_worker: 16
run-list-objects:
  page_size: 500
run-put-object: {}
`

func mustFile(t *testing.T, s string) *File {
	f, err := ParseFile([]byte(s))
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func TestMergePrecedence(t *testing.T) {
	f := mustFile(t, sample)
	c, err := Resolve(f, "load")
	if err != nil {
		t.Fatal(err)
	}
	if c.NumObjects != 5000 || c.ThreadsPerWorker != 16 || c.WorkerCount != 2 || c.PaddingBytes != 16 {
		t.Fatalf("%+v", c)
	}
	c, _ = Resolve(f, "run-put-object")
	if c.ThreadsPerWorker != 8 {
		t.Fatalf("base should win when no override: %+v", c)
	}
	// CLI beats file.
	ov := NewOverrides()
	ov.vals["threads_per_worker"] = "3"
	if err := ov.Apply(&c); err != nil || c.ThreadsPerWorker != 3 {
		t.Fatalf("%v %+v", err, c)
	}
}

func TestStrictness(t *testing.T) {
	if _, err := ParseFile([]byte("bogus: {}")); err == nil {
		t.Fatal("unknown section must fail")
	}
	f := mustFile(t, "base:\n  nope: 1\n")
	if _, err := Resolve(f, "load"); err == nil {
		t.Fatal("unknown key must fail")
	}
	f = mustFile(t, "base:\n  num_objects: abc\n")
	if _, err := Resolve(f, "load"); err == nil {
		t.Fatal("wrong type must fail")
	}
	ov := NewOverrides()
	ov.vals["num_objects"] = "x"
	c := Defaults()
	if err := ov.Apply(&c); err == nil {
		t.Fatal("bad CLI value must fail")
	}
}

func TestOpsDurationRules(t *testing.T) {
	c := Defaults()
	if err := c.Validate("run-put-object"); err == nil {
		t.Fatal("neither ops nor duration must fail")
	}
	c.Ops, c.Duration = 10, "5s"
	if err := c.Validate("run-put-object"); err == nil {
		t.Fatal("both must fail")
	}
	c.Duration = ""
	if err := c.Validate("run-put-object"); err != nil {
		t.Fatal(err)
	}
	c.Duration = "bogus"
	c.Ops = 0
	if err := c.Validate("run-put-object"); err == nil {
		t.Fatal("bad duration")
	}
	c.Duration = "30s"
	if err := c.Validate("load"); err == nil {
		t.Fatal("duration invalid for load")
	}
	// CLI --ops replaces a duration from the file.
	c = Defaults()
	c.Duration = "30s"
	ov := NewOverrides()
	ov.vals["ops"] = "100"
	ov.Apply(&c)
	if c.Duration != "" || c.Ops != 100 {
		t.Fatalf("%+v", c)
	}
	c = Defaults()
	if err := c.Validate("run-put-object-overwrite"); err == nil {
		t.Fatal("overwrite needs num_objects_to_overwrite")
	}
}

func TestCommandRulesAndWorkerJSON(t *testing.T) {
	c := Defaults()
	c.WorkerCount, c.NumBuckets = 8, 3
	w := c.ApplyCommandRules("run-list-objects")
	if c.WorkerCount != 3 || len(w) != 1 {
		t.Fatalf("capping failed: %+v %v", c.WorkerCount, w)
	}
	c = Defaults()
	c.WorkerCount, c.ThreadsPerWorker, c.SingleThreadMode = 4, 8, true
	c.ApplyCommandRules("run-delete-multi")
	if c.WorkerCount != 1 || c.ThreadsPerWorker != 1 {
		t.Fatal("single thread mode")
	}
	c.Duration = "2s"
	b, err := c.WorkerJSON()
	if err != nil || !contains(string(b), `"duration_ms":2000`) || contains(string(b), "fdb") {
		t.Fatalf("%s %v", b, err)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
