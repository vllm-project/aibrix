#!/usr/bin/env python3
import http.client
import json
import threading
import unittest

from router import Handler, decide


def request(model="premium-model", candidates=None, policy_mode="Advisory"):
    return {
        "apiVersion": "routing.aibrix.ai/v1alpha1",
        "kind": "ReplicaSelectionRequest",
        "metadata": {"requestId": "req-1"},
        "spec": {
            "model": model,
            "policyMode": policy_mode,
            "candidates": candidates or [],
        },
    }


def candidate(name, zone=None, accelerator=None):
    attributes = {}
    if zone:
        attributes["topology.kubernetes.io/zone"] = zone
    if accelerator:
        attributes["routing.example.com/accelerator-class"] = accelerator
    return {"id": "default/" + name, "ports": [8000], "attributes": attributes}


class RouterTests(unittest.TestCase):
    def test_prefers_h100_in_zone(self):
        result = decide(request(candidates=[
            candidate("b", "zone-b", "h100"),
            candidate("a", "zone-a", "h100"),
            candidate("c", "zone-a", "a100"),
        ]), "zone-a")
        self.assertEqual("default/a", result["status"]["target"]["id"])

    def test_tie_breaks_by_id(self):
        result = decide(request(model="ordinary", candidates=[
            candidate("b", "zone-a", "a100"),
            candidate("a", "zone-a", "a100"),
        ]), "zone-a")
        self.assertEqual("default/a", result["status"]["target"]["id"])

    def test_no_premium_candidate_abstains(self):
        result = decide(request(candidates=[candidate("a", "zone-a", "a100")]), "zone-a")
        self.assertEqual("NoDecision", result["status"]["decision"])

    def test_missing_attribute_abstains(self):
        result = decide(request(candidates=[candidate("a")]), "zone-a")
        self.assertEqual("NoDecision", result["status"]["decision"])

    def test_authoritative_no_match_is_denied(self):
        result = decide(
            request(candidates=[candidate("a")], policy_mode="Authoritative"),
            "zone-a",
        )
        self.assertEqual("Denied", result["status"]["decision"])

    def test_invalid_envelope(self):
        with self.assertRaises(ValueError):
            decide({"apiVersion": "wrong"})

    def test_malformed_shapes_raise_value_error(self):
        malformed = [
            [],
            request(candidates=["not-an-object"]),
            request(candidates=[{"ports": [8000]}]),
            request(candidates=[{"id": "default/a", "ports": "8000"}]),
        ]
        for document in malformed:
            with self.subTest(document=document), self.assertRaises(ValueError):
                decide(document)


class HandlerTests(unittest.TestCase):
    def test_malformed_shapes_return_http_400(self):
        from http.server import ThreadingHTTPServer

        server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        self.addCleanup(server.server_close)
        self.addCleanup(server.shutdown)

        for document in ([], request(candidates=[{"ports": [8000]}])):
            with self.subTest(document=document):
                connection = http.client.HTTPConnection(*server.server_address, timeout=3)
                connection.request(
                    "POST",
                    "/v1alpha1/select",
                    body=json.dumps(document),
                    headers={"Content-Type": "application/json"},
                )
                response = connection.getresponse()
                self.assertEqual(400, response.status)
                self.assertEqual("application/problem+json", response.getheader("Content-Type"))
                response.read()
                connection.close()


if __name__ == "__main__":
    unittest.main()
