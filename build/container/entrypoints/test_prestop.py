"""Exercise the hook with BusyBox and a local metrics server (no cluster needed).

Run: python3 build/container/entrypoints/test_prestop.py
Requires Docker and busybox:1.37.0-musl (or AIBRIX_PRESTOP_TEST_IMAGE).
"""

import http.server
import os
from pathlib import Path
import subprocess
import threading
import time
import unittest


SCRIPT = Path(__file__).with_name("prestop.sh").resolve()
IMAGE = os.environ.get("AIBRIX_PRESTOP_TEST_IMAGE", "busybox:1.37.0-musl")


def sample(value, pod="gateway-test"):
    return f'gateway_in_flight_requests{{gateway_pod="{pod}"}} {value}\n'


class PreStopTests(unittest.TestCase):
    def run_exec_hook(self, *, pid1_user="65532:65532"):
        container = subprocess.run(
            [
                "docker", "run", "-d", "--rm", "--pull=never",
                "--user", pid1_user, "--read-only", "--cap-drop=ALL",
                "--entrypoint=/bin/busybox",
                "-v", f"{SCRIPT}:/prestop.sh:ro",
                IMAGE, "sleep", "60",
            ],
            capture_output=True, text=True, timeout=15,
        )
        self.assertEqual(container.returncode, 0, container.stderr)
        container_id = container.stdout.strip()
        try:
            # Like a lifecycle hook, exec has separate output from PID 1.
            # Missing POD_NAME also checks that validation errors are visible.
            result = subprocess.run(
                ["docker", "exec", "--user", "65532:65532", container_id,
                 "/bin/busybox", "sh", "/prestop.sh"],
                capture_output=True, text=True, timeout=15,
            )
            logs = subprocess.run(
                ["docker", "logs", container_id],
                capture_output=True, text=True, timeout=15,
            )
            self.assertEqual(logs.returncode, 0, logs.stderr)
            self.assertEqual(result.returncode, 2, result.stderr)
            return result, logs.stdout + logs.stderr
        finally:
            subprocess.run(
                ["docker", "rm", "-f", container_id],
                capture_output=True, text=True, timeout=15, check=True,
            )

    def test_exec_hook_logs_are_visible_in_container_logs(self):
        result, log = self.run_exec_hook()
        self.assertIn("gateway preStop: expected at most four arguments", log)
        self.assertEqual(result.stderr, "")

    def test_exec_hook_falls_back_when_pid1_stderr_is_inaccessible(self):
        # An unprivileged exec cannot open the root-owned PID 1 descriptors.
        result, log = self.run_exec_hook(pid1_user="0:0")
        self.assertIn("gateway preStop: expected at most four arguments", result.stderr)
        self.assertNotIn("gateway preStop:", log)
        self.assertNotIn("Permission denied", result.stderr)

    def run_hook(self, responses, *, delay=0, timeout=8, poll=1, pod="gateway-test", expected_status=0):
        requests = []

        class Handler(http.server.BaseHTTPRequestHandler):
            def do_GET(self):
                index = min(len(requests), len(responses) - 1)
                requests.append(time.monotonic())
                response = responses[index]
                if response == "hang":
                    # The whole fetch must time out without waiting for EOF.
                    self.send_response(200)
                    self.end_headers()
                    try:
                        for _ in range(60):
                            self.wfile.write(b"# keepalive\n")
                            self.wfile.flush()
                            time.sleep(0.1)
                    except (BrokenPipeError, ConnectionResetError):
                        pass
                    return
                status, body = response if isinstance(response, tuple) else (200, response)
                self.send_response(status)
                self.end_headers()
                self.wfile.write(body.encode())

            def log_message(self, *_):
                pass

        server = http.server.ThreadingHTTPServer(("0.0.0.0", 0), Handler)
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        started = time.monotonic()
        try:
            result = subprocess.run(
                [
                    "docker", "run", "--rm", "--pull=never",
                    "--add-host=host.docker.internal:host-gateway",
                    "--user=65532:65532", "--read-only", "--cap-drop=ALL",
                    "--entrypoint=/bin/busybox",
                    "-e", f"POD_NAME={pod}",
                    "-e", "AIBRIX_PRESTOP_BUSYBOX=/bin/busybox",
                    "-e", "http_proxy=http://127.0.0.1:1",
                    "-v", f"{SCRIPT}:/prestop.sh:ro",
                    IMAGE, "sh", "/prestop.sh",
                    f"http://host.docker.internal:{server.server_port}/metrics",
                    str(delay), str(timeout), str(poll),
                ],
                capture_output=True, text=True, timeout=15,
            )
            elapsed = time.monotonic() - started
        finally:
            server.shutdown()
            server.server_close()
            thread.join()
        self.assertEqual(result.returncode, expected_status, result.stderr)
        self.assertLess(elapsed, timeout + 3, result.stderr)
        return result.stderr, requests, started

    def test_waits_for_propagation_and_three_zero_samples(self):
        log, requests, started = self.run_hook([sample(0)], delay=1)
        self.assertIn("drain complete", log)
        self.assertEqual(len(requests), 3)
        self.assertGreaterEqual(requests[0] - started, 1)

    def test_busy_resets_zero_count(self):
        log, requests, _ = self.run_hook([sample(0), sample(2), sample(0)])
        self.assertIn("requests still in flight: 2\n", log)
        self.assertIn("drain complete", log)
        self.assertEqual(len(requests), 5)

    def test_invalid_or_missing_metrics_never_count_as_zero(self):
        for body in ["", sample(0, pod="other-pod"), sample("NaN"),
                     sample("-1"), sample("oops"), sample(0) + sample(0)]:
            with self.subTest(body=body):
                log, _, _ = self.run_hook([body], timeout=4)
                self.assertIn("sample missing or invalid", log)
                self.assertIn("deadline reached", log)
                self.assertNotIn("drain complete", log)

    def test_zero_sample_with_other_metrics_and_scientific_notation(self):
        body = sample("0e+00") + sample(9, pod="other-pod") + "other_metric 123\n"
        log, requests, _ = self.run_hook([body])
        self.assertIn("drain complete", log)
        self.assertNotIn("requests still in flight", log)
        self.assertEqual(len(requests), 3)

    def test_busy_requests_reach_deadline(self):
        log, _, _ = self.run_hook([sample(1)], timeout=2)
        self.assertIn("requests still in flight: 1\n", log)
        self.assertIn("deadline reached", log)

    def test_fetch_failure_resets_zero_count(self):
        log, requests, _ = self.run_hook([sample(0), (503, "unavailable"), sample(0)])
        self.assertIn("metrics fetch failed", log)
        self.assertIn("drain complete", log)
        self.assertEqual(len(requests), 5)

    def test_missing_sample_resets_zero_count(self):
        log, requests, _ = self.run_hook([sample(0), "# no sample\n", sample(0)])
        self.assertIn("sample missing or invalid", log)
        self.assertIn("drain complete", log)
        self.assertEqual(len(requests), 5)

    def test_fetch_is_bounded_even_with_trickling_response(self):
        log, _, _ = self.run_hook(["hang"], timeout=2)
        self.assertIn("metrics fetch failed or timed out", log)
        self.assertIn("deadline reached", log)

    def test_invalid_configuration(self):
        for delay, timeout, poll, pod in [(0, 5, 0, "gateway-test"),
                                          (5, 5, 1, "gateway-test"),
                                          ("01", 5, 1, "gateway-test"),
                                          (0, 5, 1, "")]:
            with self.subTest(delay=delay, timeout=timeout, poll=poll, pod=pod):
                _, requests, _ = self.run_hook(
                    [sample(0)], delay=delay, timeout=timeout, poll=poll, pod=pod,
                    expected_status=2,
                )
                self.assertEqual(requests, [])


if __name__ == "__main__":
    unittest.main(verbosity=2)
