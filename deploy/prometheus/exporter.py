#!/usr/bin/env python3
"""Minimal deterministic Prometheus sample exporter for local MVP verification."""

from http.server import BaseHTTPRequestHandler, HTTPServer
import math
import time

START = time.time()


def render_metrics() -> str:
    elapsed = max(1.0, time.time() - START)
    req_200 = int(elapsed * 42)
    req_500 = int(elapsed * 1.2)
    b01 = int(elapsed * 20)
    b05 = int(elapsed * 38)
    b10 = int(elapsed * 41)
    binf = int(elapsed * 43)
    mem_bytes = int(268435456 + 10485760 * math.sin(elapsed / 30.0))

    lines = [
        '# HELP http_requests_total Total number of HTTP requests handled by service.',
        '# TYPE http_requests_total counter',
        f'http_requests_total{{service="checkout-api",method="POST",status="200"}} {req_200}',
        f'http_requests_total{{service="checkout-api",method="POST",status="500"}} {req_500}',
        '# HELP http_request_duration_seconds HTTP request latency histogram in seconds.',
        '# TYPE http_request_duration_seconds histogram',
        f'http_request_duration_seconds_bucket{{service="checkout-api",le="0.1"}} {b01}',
        f'http_request_duration_seconds_bucket{{service="checkout-api",le="0.5"}} {b05}',
        f'http_request_duration_seconds_bucket{{service="checkout-api",le="1.0"}} {b10}',
        f'http_request_duration_seconds_bucket{{service="checkout-api",le="+Inf"}} {binf}',
        f'http_request_duration_seconds_sum{{service="checkout-api"}} {elapsed * 11.4:.2f}',
        f'http_request_duration_seconds_count{{service="checkout-api"}} {binf}',
        '# HELP process_resident_memory_bytes Resident memory size in bytes.',
        '# TYPE process_resident_memory_bytes gauge',
        f'process_resident_memory_bytes{{service="checkout-api",instance="checkout-1"}} {mem_bytes}',
    ]
    return "\n".join(lines) + "\n"


class Handler(BaseHTTPRequestHandler):
    def do_GET(self):
        if self.path != "/metrics":
            self.send_response(404)
            self.end_headers()
            return
        body = render_metrics().encode("utf-8")
        self.send_response(200)
        self.send_header("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, format, *args):
        return


if __name__ == "__main__":
    HTTPServer(("0.0.0.0", 9108), Handler).serve_forever()
