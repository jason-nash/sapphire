import json
from pathlib import Path
import subprocess
import sys
import unittest

SAMPLER = Path(__file__).resolve().parent / "build" / "footprint"


@unittest.skipUnless(sys.platform == "darwin" and SAMPLER.exists(), "requires the built macOS sampler")
class SamplerTests(unittest.TestCase):
    def test_cpu_time_matches_the_measured_process_clock(self):
        code = """
import sys, time
deadline = time.process_time() + .15
while time.process_time() < deadline:
    pass
print(time.process_time_ns(), flush=True)
sys.stdin.read(1)
"""
        process = subprocess.Popen(
            [sys.executable, "-c", code], stdin=subprocess.PIPE, stdout=subprocess.PIPE, text=True
        )
        try:
            own_clock = int(process.stdout.readline())
            sample = json.loads(subprocess.check_output([str(SAMPLER), str(process.pid), str(process.pid)]))
            self.assertEqual(len(sample), 1, "overlapping roots must not double-count memory or CPU")
            measured = sample[0]["cpuNs"]
            self.assertGreater(measured, own_clock * .9)
            self.assertLess(measured, own_clock * 1.2 + 20_000_000)
            self.assertGreater(sample[0]["footprint"], 0)
        finally:
            process.stdin.close()
            process.stdout.close()
            process.wait(timeout=5)


if __name__ == "__main__":
    unittest.main()
