import copy
import unittest

from resources import METRICS, PHASES, WORKLOADS, compare_records, summarize


class ResourceTests(unittest.TestCase):
    def records(self):
        records = []
        for workload in WORKLOADS:
            for phase in PHASES:
                for backend in ("sapphire", "orbstack"):
                    records.append({
                        "backend": backend, "workload": workload, "phase": phase,
                        "isolation": "exclusive", "isolationEvidence": "dedicated test host",
                        "specificationSHA256": workload, "host": {"machine": "arm64"},
                        "summary": {**dict.fromkeys(METRICS, 100), "stableProcessSet": True, "durationSeconds": 120},
                    })
        return records

    def test_requires_all_workloads_and_phases(self):
        self.assertFalse(compare_records(self.records()[:-1])["passed"])

    def test_confounded_or_mismatched_baseline_cannot_pass(self):
        for modification in (
            {"isolation": "shared"}, {"isolationEvidence": ""},
            {"specificationSHA256": "different"}, {"host": {"machine": "other"}},
            {"summary": {"stableProcessSet": False, "durationSeconds": 120}},
        ):
            records = self.records()
            records[0].update(modification)
            self.assertFalse(compare_records(records)["passed"], modification)

    def test_any_resource_regression_fails(self):
        records = self.records()
        records[0]["summary"]["footprintP95Bytes"] += 1
        self.assertFalse(compare_records(records)["passed"])
        self.assertTrue(compare_records(self.records())["passed"])

    def test_duplicate_capture_cannot_mask_a_regression(self):
        records = self.records()
        records.append(copy.deepcopy(records[0]))
        self.assertFalse(compare_records(records)["passed"])

    def test_pid_reuse_invalidates_interval_instead_of_subtracting_counters(self):
        def sample(elapsed, start, cpu):
            return {"elapsed": elapsed, "processes": [{
                "pid": 42, "start": start, "cpuNs": cpu, "footprint": 1024,
                "idleWakeups": 0, "interruptWakeups": 0,
            }]}
        result = summarize([sample(0, 10, 100), sample(1, 20, 1)])
        self.assertFalse(result["stableProcessSet"])
        self.assertIsNone(result["cpuMeanPercent"])

    def test_cpu_counter_is_nanoseconds_and_not_rss(self):
        samples = [
            {"elapsed": elapsed, "processes": [{
                "pid": 42, "start": 10, "cpuNs": cpu, "footprint": footprint, "rss": 99999999,
                "idleWakeups": wakeups, "interruptWakeups": wakeups,
            }]} for elapsed, cpu, footprint, wakeups in ((0, 0, 1024, 0), (2, 100000000, 2048, 4))
        ]
        result = summarize(samples)
        self.assertEqual(result["cpuMeanPercent"], 5)
        self.assertEqual(result["footprintP95Bytes"], 2048)
        self.assertEqual(result["idleWakeupsPerSecond"], 2)


if __name__ == "__main__":
    unittest.main()
