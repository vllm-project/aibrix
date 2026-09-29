#!/usr/bin/env python3
import json
import os
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

API_VERSION = "routing.aibrix.ai/v1alpha1"
MEDIA_TYPE = "application/vnd.aibrix.external-routing+json;version=v1alpha1"


def decide(document, preferred_zone=None, premium_model="premium-model"):
    if not isinstance(document, dict):
        raise ValueError("request body must be an object")
    if document.get("apiVersion") != API_VERSION or document.get("kind") != "ReplicaSelectionRequest":
        raise ValueError("unsupported request envelope")
    metadata = document.get("metadata")
    spec = document.get("spec")
    if not isinstance(metadata, dict) or not isinstance(spec, dict):
        raise ValueError("metadata and spec must be objects")
    request_id = metadata.get("requestId")
    model = spec.get("model")
    policy_mode = spec.get("policyMode")
    candidates = spec.get("candidates")
    if not isinstance(request_id, str) or not request_id:
        raise ValueError("requestId is required")
    if not isinstance(model, str) or not model:
        raise ValueError("model is required")
    if policy_mode not in ("Advisory", "Authoritative"):
        raise ValueError("policyMode must be Advisory or Authoritative")
    if not isinstance(candidates, list) or not candidates:
        raise ValueError("requestId and candidates are required")
    for candidate in candidates:
        if not isinstance(candidate, dict):
            raise ValueError("each candidate must be an object")
        candidate_id = candidate.get("id")
        ports = candidate.get("ports")
        attributes = candidate.get("attributes")
        if not isinstance(candidate_id, str) or not candidate_id:
            raise ValueError("candidate id is required")
        if (
            not isinstance(ports, list)
            or not ports
            or any(
                isinstance(port, bool)
                or not isinstance(port, int)
                or port < 1
                or port > 65535
                for port in ports
            )
        ):
            raise ValueError(
                "candidate ports must be non-empty integers in range 1-65535"
            )
        if attributes is not None and not isinstance(attributes, dict):
            raise ValueError("candidate attributes must be an object")
    candidates = sorted(candidates, key=lambda item: item["id"])

    eligible = candidates
    if model == premium_model:
        eligible = [
            item for item in eligible
            if (item.get("attributes") or {}).get("routing.example.com/accelerator-class") == "h100"
        ]
    if preferred_zone:
        zonal = [
            item for item in eligible
            if (item.get("attributes") or {}).get("topology.kubernetes.io/zone") == preferred_zone
        ]
        if zonal:
            eligible = zonal
    if not eligible:
        if policy_mode == "Authoritative":
            status = {"decision": "Denied", "reason": "NoApplicablePolicy"}
        else:
            status = {"decision": "NoDecision", "reason": "NoApplicablePolicy"}
    else:
        winner = eligible[0]
        ports = winner.get("ports") or []
        if not ports:
            raise ValueError("candidate ports are required")
        status = {
            "decision": "Selected",
            "target": {"id": winner["id"], "port": sorted(ports)[0]},
        }
    return {
        "apiVersion": API_VERSION,
        "kind": "ReplicaSelectionResponse",
        "metadata": {"requestId": request_id},
        "status": status,
    }


class Handler(BaseHTTPRequestHandler):
    def do_POST(self):
        if self.path != "/v1alpha1/select":
            self.send_error(404)
            return
        try:
            length = int(self.headers.get("Content-Length", "0"))
            if length <= 0 or length > 262144:
                raise ValueError("invalid body size")
            request = json.loads(self.rfile.read(length))
            response = decide(request, os.getenv("PREFERRED_ZONE"))
            body = json.dumps(response, separators=(",", ":")).encode()
            self.send_response(200)
            self.send_header("Content-Type", MEDIA_TYPE)
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)
        except (ValueError, json.JSONDecodeError) as error:
            body = json.dumps({"title": str(error), "status": 400}).encode()
            self.send_response(400)
            self.send_header("Content-Type", "application/problem+json")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)

    def log_message(self, _format, *_args):
        return


if __name__ == "__main__":
    ThreadingHTTPServer(("0.0.0.0", 8080), Handler).serve_forever()
