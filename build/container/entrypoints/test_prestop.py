"""Smoke-test the Go preStop command in the shipped, unprivileged gateway image.

Core drain cases run without Docker: go test ./pkg/plugins/gateway/drain ./cmd/plugins
Build: docker build -f build/container/Dockerfile.gateway -t aibrix/gateway-plugins:prestop-test .
Run: python3 build/container/entrypoints/test_prestop.py
"""

import http.server
import os
import subprocess
import threading
import unittest


IMAGE = os.environ.get("AIBRIX_PRESTOP_TEST_IMAGE", "aibrix/gateway-plugins:prestop-test")


class PreStopImageTests(unittest.TestCase):
    def test_exec_hook_logging(self):
        for pid1_user in ["65532:65532", "0:0"]:
            with self.subTest(pid1_user=pid1_user):
                result = subprocess.run(
                    ["docker", "run", "-d", "--rm", "--pull=never", "--read-only",
                     "--cap-drop=ALL", "--user", pid1_user, IMAGE, "prestop",
                     "--metrics-url=http://127.0.0.1:1/metrics", "--propagation-delay=0s",
                     "--timeout=60s", "--poll-interval=1s"],
                    capture_output=True, text=True, timeout=15, check=True,
                )
                container = result.stdout.strip()
                try:
                    hook = subprocess.run(
                        ["docker", "exec", "--user", "65532:65532", container,
                         "/gateway-plugins", "prestop", "extra"],
                        capture_output=True, text=True, timeout=15,
                    )
                    self.assertEqual(hook.returncode, 2, hook.stdout + hook.stderr)
                    logs = subprocess.run(
                        ["docker", "logs", container], capture_output=True,
                        text=True, timeout=15, check=True,
                    )
                    message = "unexpected positional arguments"
                    if pid1_user == "65532:65532":
                        self.assertIn(message, logs.stdout + logs.stderr)
                        self.assertNotIn(message, hook.stderr)
                    else:
                        self.assertIn(message, hook.stderr)
                        self.assertNotIn(message, logs.stdout + logs.stderr)
                finally:
                    subprocess.run(
                        ["docker", "rm", "-f", container], capture_output=True,
                        text=True, timeout=15, check=True,
                    )

    def test_fetches_metrics_without_shell_or_proxy(self):
        requests = []

        class Handler(http.server.BaseHTTPRequestHandler):
            def do_GET(self):
                requests.append(self.path)
                self.send_response(200)
                self.end_headers()
                self.wfile.write(b'gateway_in_flight_requests{gateway_pod="test"} 0\n')

            def log_message(self, *_):
                pass

        server = http.server.ThreadingHTTPServer(("0.0.0.0", 0), Handler)
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        try:
            result = subprocess.run(
                ["docker", "run", "--rm", "--pull=never", "--read-only", "--cap-drop=ALL",
                 "--user=65532:65532", "--add-host=host.docker.internal:host-gateway",
                 "-e", "http_proxy=http://127.0.0.1:1", "-e", "HTTP_PROXY=http://127.0.0.1:1",
                 "-e", "KUBECONFIG=/does-not-exist", "-e", "REDIS_HOST=does-not-exist.invalid",
                 IMAGE, "prestop",
                 f"--metrics-url=http://host.docker.internal:{server.server_port}/metrics",
                 "--propagation-delay=0s", "--timeout=8s", "--poll-interval=100ms"],
                capture_output=True, text=True, timeout=15,
            )
            self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
            self.assertIn("drain complete", result.stdout + result.stderr)
            self.assertEqual(requests, ["/metrics"] * 3)
        finally:
            server.shutdown()
            server.server_close()
            thread.join()


if __name__ == "__main__":
    unittest.main(verbosity=2)
