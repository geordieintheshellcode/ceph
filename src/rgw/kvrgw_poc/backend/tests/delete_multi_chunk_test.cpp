// -*- mode:C++; tab-width:8; c-basic-offset:2;
// vim: ts=8 sw=2 smarttab
/*
 * This is free software; you can redistribute it and/or
 * modify it under the terms of the GNU Lesser General Public
 * License version 2.1, as published by the Free Software
 * Foundation.  See file COPYING.
 */

// delete_multi_chunk_test: delete_multi works in chunks of kMaxBatchSize keys
// that reuse one array of per-key contexts. A key that is absent in a later
// chunk must not see the value an existing key left in the same slot in an
// earlier chunk.
//
// Versioned bucket: put 20 keys "a-NN", then delete_multi 40 keys -- the 20
// "a-NN" (chunk 1) and 20 never-written "b-NN" (chunk 2, same slots). Deleting
// a key that does not exist must only create a delete marker, so every "b-"
// key must end with exactly one version, a delete marker. With a stale
// context the "b-" key is instead treated as holding the slot's "a-" object:
// a non-delete-marker version of that data appears under the "b-" key.
// Needs a live FDB cluster.

#include "constants.hpp"
#include "error_codes.hpp"
#include "kvrgw_runtime.hpp"
#include "service_impl.hpp"

#include <cstdio>
#include <cstdlib>
#include <filesystem>
#include <iostream>
#include <map>
#include <string>
#include <unistd.h>
#include <vector>

namespace {

int g_fail = 0;

#define CHECK(c)                                                          \
  do {                                                                    \
    if (!(c)) {                                                           \
      std::cerr << "FAIL: " #c " at line " << __LINE__ << "\n";           \
      ++g_fail;                                                           \
    }                                                                     \
  } while (0)

std::string key(const char *prefix, int i)
{
  char buf[16];
  std::snprintf(buf, sizeof(buf), "%s-%02d", prefix, i);
  return buf;
}

int run(kvrgw::KvRgwServiceImpl &svc)
{
  using namespace kvrgw;
  static_assert(kMaxBatchSize == 20, "the test lays out one full chunk per prefix");

  tenant_id_t tenant{};
  bool exists = false;
  CHECK(svc.resolve_tenant("delete-multi-chunk-test", &exists, &tenant) == KVRGW_ERR_OK);
  if (!exists) {
    CHECK(svc.add_tenant("delete-multi-chunk-test", &tenant) == KVRGW_ERR_OK);
  }
  const std::string bucket = "dmc-" + std::to_string(getpid()) + "-" + std::to_string(time(nullptr));
  CHECK(svc.create_bucket(tenant, bucket) == KVRGW_ERR_OK);
  CHECK(svc.put_bucket_versioning(tenant, bucket, VERSIONING_ENABLED) == KVRGW_ERR_OK);

  // Chunk 1: 20 existing keys.
  const std::string body = "payload";
  for (int i = 0; i < kMaxBatchSize; ++i) {
    const auto r = svc.put_object(tenant, bucket, key("a", i),
                                  reinterpret_cast<const uint8_t *>(body.data()), body.size(),
                                  "", 0, nullptr, {}, {});
    CHECK(r.error_code == KVRGW_ERR_OK);
  }

  // One delete_multi: the 20 existing keys, then 20 that were never written.
  std::vector<std::string> keys;
  for (int i = 0; i < kMaxBatchSize; ++i) keys.push_back(key("a", i));
  for (int i = 0; i < kMaxBatchSize; ++i) keys.push_back(key("b", i));
  std::vector<KvRgwServiceImpl::DeleteMultiKeyOutcome> out;
  CHECK(svc.delete_multi(tenant, bucket, keys, {}, &out) == KVRGW_ERR_OK);
  CHECK(out.size() == keys.size());

  // Every "b-" key: exactly one version, a delete marker.
  std::map<std::string, std::pair<int, int>> b_versions;  // key -> (delete markers, others)
  KvRgwServiceImpl::ListObjectVersionsResult res;
  std::string key_marker;
  version_id_t vid_marker{};
  do {
    res = {};
    CHECK(svc.list_object_versions(tenant, bucket, "b-", 1000, key_marker, vid_marker, &res) ==
          KVRGW_ERR_OK);
    for (const auto &v : res.versions) {
      auto &c = b_versions[v.key];
      (v.is_delete_marker ? c.first : c.second)++;
    }
    key_marker = res.next_key_marker;
    vid_marker = res.next_version_id_marker;
  } while (res.is_truncated);

  CHECK(b_versions.size() == static_cast<size_t>(kMaxBatchSize));
  int stray = 0;
  for (const auto &[k, c] : b_versions) {
    if (c.first != 1 || c.second != 0) {
      ++stray;
      std::cerr << "  " << k << ": " << c.first << " delete marker(s), " << c.second
                << " other version(s) (expected 1 delete marker only)\n";
    }
  }
  CHECK(stray == 0);
  return g_fail == 0 ? 0 : 1;
}

}  // namespace

int main()
{
  char admin_sock[64];
  std::snprintf(admin_sock, sizeof(admin_sock), "/tmp/kvrgw-dmc-test-%d.sock",
                static_cast<int>(getpid()));
  setenv("KVRGW_ADMIN_SOCKET", admin_sock, 1);
  setenv("KVRGW_BATCH_SIZE", "1", 0);

  kvrgw::KvRgwStartOptions opts;
  opts.perf_mode = false;
  opts.data_root = "/tmp/kvrgw-dmc-test-data-" + std::to_string(getpid());
  std::error_code fs_ec;
  std::filesystem::create_directories(opts.data_root, fs_ec);

  kvrgw::KvRgwRuntime runtime;
  if (!runtime.start(opts)) {
    std::cerr << "FAIL: runtime start failed\n";
    return 1;
  }
  const int rc = run(runtime.service());
  runtime.stop();
  std::filesystem::remove_all(opts.data_root, fs_ec);
  std::cout << (rc == 0 ? "PASS" : "FAIL") << ": delete_multi_chunk_test\n";
  return rc;
}
