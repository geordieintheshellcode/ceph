* Use clang-format (there's a clang-format file at the repo root)
* Use json logging for golang and cpp harness for go use the same logger k11 uses.
  For cpp you could pull in spdlog as it's header only. Logs should include timetamp,
  source=worker_id-$i,controller, etc.
* What the hell are is "FDB by role" telling us
* `Client ops (cluster total, merged across workers)` is stupid
