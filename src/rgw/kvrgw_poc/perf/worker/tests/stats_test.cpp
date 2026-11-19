#include "stats.hpp"
#include "test_util.hpp"

using namespace perfw;

int main()
{
  StatsHub hub(2);
  hub.thread(0).record(Metric::PutObject, 100);
  hub.thread(1).record(Metric::PutObject, 300);
  hub.thread(1).record_error(Metric::PutObject);
  hub.thread(0).add_units(Metric::KeysListed, 50);
  auto w = hub.collect_window();
  auto &p = w[static_cast<size_t>(Metric::PutObject)];
  CHECK(p.count == 2 && p.errors == 1 && p.sum_micros == 400);
  CHECK(p.hist[hist_index(100)] == 1 && p.hist[hist_index(300)] == 1);
  CHECK(w[static_cast<size_t>(Metric::KeysListed)].count == 50);
  // Window resets; cumulative accumulates.
  hub.thread(0).record(Metric::PutObject, 100);
  auto w2 = hub.collect_window();
  CHECK(w2[static_cast<size_t>(Metric::PutObject)].count == 1);
  CHECK(hub.cumulative()[static_cast<size_t>(Metric::PutObject)].count == 3);
  std::puts("stats_test OK");
}
