#!/usr/bin/env python3
import unittest

from router import decide


def request(model="premium-model", candidates=None):
    return {
        "apiVersion": "routing.aibrix.ai/v1alpha1",
        "kind": "ReplicaSelectionRequest",
        "metadata": {"requestId": "req-1"},
        "spec": {
            "model": model,
            "policyMode": "Advisory",
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

    def test_invalid_envelope(self):
        with self.assertRaises(ValueError):
            decide({"apiVersion": "wrong"})


if __name__ == "__main__":
    unittest.main()

