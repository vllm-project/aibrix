from aibrix.metadata.core.metrics import T
from aibrix.metadata.core.metrics.udp import StatsdSink, _split_udp_addr


class _FakeSocket:
    def __init__(self) -> None:
        self.sent: list[tuple[bytes, tuple]] = []
        self.closed = False

    def sendto(self, payload: bytes, sockaddr: tuple) -> None:
        self.sent.append((payload, sockaddr))

    def close(self) -> None:
        self.closed = True


def test_split_udp_addr_rejects_missing_port():
    try:
        _split_udp_addr("metrics-host")
    except ValueError as exc:
        assert "host:port" in str(exc)
    else:
        raise AssertionError("expected missing port to be rejected")


def test_statsd_sink_defers_resolution_until_first_send(monkeypatch):
    resolution_calls: list[tuple[str, int, int]] = []
    fake_socket = _FakeSocket()

    def _fake_getaddrinfo(host, port, type):
        resolution_calls.append((host, port, type))
        return [(2, None, None, None, ("127.0.0.1", 8125))]

    monkeypatch.setattr(
        "aibrix.metadata.core.metrics.udp.socket.getaddrinfo", _fake_getaddrinfo
    )
    monkeypatch.setattr(
        "aibrix.metadata.core.metrics.udp.socket.socket",
        lambda family, sock_type: fake_socket,
    )

    sink = StatsdSink("metrics.example:8125", prefix="svc")

    assert resolution_calls == []

    sink.counter("requests", 2, T("endpoint", "/health"))

    assert resolution_calls == [("metrics.example", 8125, 2)]
    assert fake_socket.sent == [
        (b"svc.requests:2|c", ("127.0.0.1", 8125)),
    ]


def test_statsd_sink_refreshes_resolution_after_ttl(monkeypatch):
    fake_socket = _FakeSocket()
    monotonic_values = iter([0.0, 5.0, 31.0])
    resolved_addresses = iter(
        [
            (2, ("127.0.0.1", 8125)),
            (2, ("127.0.0.2", 8125)),
        ]
    )
    resolution_calls: list[tuple[str, int, int]] = []

    monkeypatch.setattr(
        "aibrix.metadata.core.metrics.udp.time.monotonic",
        lambda: next(monotonic_values),
    )

    def _fake_getaddrinfo(host, port, type):
        resolution_calls.append((host, port, type))
        family, sockaddr = next(resolved_addresses)
        return [(family, None, None, None, sockaddr)]

    monkeypatch.setattr(
        "aibrix.metadata.core.metrics.udp.socket.getaddrinfo", _fake_getaddrinfo
    )
    monkeypatch.setattr(
        "aibrix.metadata.core.metrics.udp.socket.socket",
        lambda family, sock_type: fake_socket,
    )

    sink = StatsdSink("metrics.example:8125")
    sink.counter("requests", 1)
    sink.counter("requests", 1)
    sink.counter("requests", 1)

    assert resolution_calls == [
        ("metrics.example", 8125, 2),
        ("metrics.example", 8125, 2),
    ]
    assert fake_socket.sent == [
        (b"requests:1|c", ("127.0.0.1", 8125)),
        (b"requests:1|c", ("127.0.0.1", 8125)),
        (b"requests:1|c", ("127.0.0.2", 8125)),
    ]


def test_statsd_sink_drops_payload_when_address_is_invalid(monkeypatch):
    socket_calls: list[tuple[int, int]] = []

    monkeypatch.setattr(
        "aibrix.metadata.core.metrics.udp.socket.socket",
        lambda family, sock_type: socket_calls.append((family, sock_type)),
    )

    sink = StatsdSink("missing-port")

    sink.counter("requests", 1)

    assert socket_calls == []
