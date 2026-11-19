#include "ops.hpp"

#include "error_codes.hpp"

#include <iostream>
#include <thread>

namespace perfw {

bool RunContext::wait_start() const
{
  while (!stopped()) {
    auto now = SteadyClock::now();
    if (now >= start_tp) return true;
    std::this_thread::sleep_for(std::min<SteadyClock::duration>(start_tp - now,
                                                                std::chrono::milliseconds(50)));
  }
  return false;
}

std::string ServiceLister::list(const std::string &prefix, uint32_t max_keys,
                                const std::string &token, ListPage *out)
{
  kvrgw::KvRgwServiceImpl::ListObjectsResult res;
  auto ec = svc_.list_objects(tenant_, bucket_, prefix, "", max_keys, token, "", &res);
  if (ec != kvrgw::KVRGW_ERR_OK) return kvrgw::kvrgw_strerror(ec);
  out->keys.clear();
  out->keys.reserve(res.objects.size());
  for (auto &o : res.objects) out->keys.push_back(std::move(o.key));
  out->truncated = res.is_truncated;
  out->next_token = std::move(res.next_continuation_token);
  return {};
}

bool put_with_retry(RunContext &ctx, ThreadStats &ts, const std::string &bucket,
                    const std::string &key)
{
  constexpr int kAttempts = 10;
  kvrgw::KvrgwErrorCode last = kvrgw::KVRGW_ERR_OK;
  for (int a = 0; a < kAttempts && !ctx.stopped(); ++a) {
    auto t0 = SteadyClock::now();
    kvrgw::KvRgwServiceImpl::PutObjectResult res;
    {
      // Server-side stats are recorded by the caller (as perf_driver does).
      kvrgw::ScopedRequestLatency lat(ctx.svc->latency_stats(), kvrgw::OpType::kPutObject);
      res = ctx.svc->put_object(ctx.tenant, bucket, key,
                                reinterpret_cast<const uint8_t *>(ctx.payload.data()),
                                ctx.payload.size(), "application/octet-stream",
                                ctx.payload.size(), nullptr, {}, {});
    }
    if (res.error_code == kvrgw::KVRGW_ERR_OK) {
      ts.record(Metric::PutObject, static_cast<uint64_t>(elapsed_us(t0)));
      return true;
    }
    last = res.error_code;
    ts.record_error(Metric::PutObject);
    static std::atomic<int> logged{0};
    if (logged.fetch_add(1) < 20) {
      std::cerr << "put_object " << bucket << "/" << key
                << " failed: " << kvrgw::kvrgw_strerror(last) << " (attempt " << a + 1 << ")\n";
    }
  }
  if (!ctx.stopped()) {
    ctx.fail(std::string("put_object failed after ") + std::to_string(kAttempts) +
             " attempts: " + kvrgw::kvrgw_strerror(last));
  }
  return false;
}

std::string verify_load_family(RunContext &ctx, const std::vector<uint64_t> &buckets,
                               const std::function<Range(uint64_t)> &range_of_bucket,
                               const std::function<bool(uint64_t, uint64_t)> &present,
                               VerifyReport *out)
{
  Failures f;
  auto prefixes = prefix_items(ctx.cfg.padding_bytes);
  for (uint64_t b : buckets) {
    ServiceLister lister(*ctx.svc, ctx.tenant, ctx.bucket(b));
    Range r = range_of_bucket(b);
    LoadSpace sp;
    sp.start = r.start;
    sp.count = r.count;
    sp.padding = ctx.cfg.padding_bytes;
    if (present) sp.expected_present = [&present, b](uint64_t id) { return present(b, id); };
    out->buckets.push_back(
        verify_bucket_load(lister, ctx.bucket(b), sp, prefixes, ctx.cfg.verify_threads, f));
  }
  out->take(f);
  return {};
}

std::string check_buckets(RunContext &ctx, const std::vector<uint64_t> &buckets,
                          bool require_versioning)
{
  for (uint64_t b : buckets) {
    bool exists = false;
    auto ec = ctx.svc->bucket_exists(ctx.tenant, ctx.bucket(b), &exists);
    if (ec != kvrgw::KVRGW_ERR_OK) {
      return "bucket_exists " + ctx.bucket(b) + ": " + kvrgw::kvrgw_strerror(ec);
    }
    if (!exists) return "bucket " + ctx.bucket(b) + " does not exist (run load first)";
    if (require_versioning) {
      kvrgw::VersioningState st{};
      ec = ctx.svc->get_bucket_versioning(ctx.tenant, ctx.bucket(b), &st);
      if (ec != kvrgw::KVRGW_ERR_OK) {
        return "get_bucket_versioning " + ctx.bucket(b) + ": " + kvrgw::kvrgw_strerror(ec);
      }
      if (st != kvrgw::VERSIONING_ENABLED) {
        return "bucket " + ctx.bucket(b) + " does not have versioning enabled";
      }
    }
  }
  return {};
}

}  // namespace perfw
