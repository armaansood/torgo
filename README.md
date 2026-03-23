# Torgo

An anonymous overlay network based on the [Tor protocol](https://svn-archive.torproject.org/svn/projects/design-paper/tor-design.pdf), written from scratch in Go. It routes traffic from a browser through a randomized 3-hop circuit of routers before sending it to the destination web server. Routers find each other using a UDP peer discovery service. It can run for multiple days in a heterogeneous environment without resource leaks or deadlock.

Built as an undergraduate project for educational purposes.

## How it works

A client configures their browser to use Torgo's local HTTP proxy. When the browser makes a request, the proxy wraps it in fixed-size 512-byte cells and sends it through a circuit of three routers before it reaches the destination server. The response travels back along the same circuit.

```
Browser → Proxy → Router 1 → Router 2 → Router 3 (exit) → Web Server
                                                        ←
```

### Circuit building

1. The proxy contacts a registration service to discover available routers
2. It picks three at random and builds a circuit incrementally:
   - Sends a `CREATE` cell to the first router
   - Sends `EXTEND` relay cells to reach the second and third routers
3. Each router stores a forwarding table that maps incoming circuit IDs to outgoing circuit IDs, so cells can be routed bidirectionally without knowing the full path

### Cell protocol

All communication uses 512-byte cells, matching the Tor spec:

```
[CircuitID: 2 bytes][Type: 1 byte][Relay header: 11 bytes][Body: up to 498 bytes]
```

Relay commands include `BEGIN` (open a stream to a destination), `DATA` (carry payload), `END` (close a stream), `EXTEND`/`EXTENDED` (build circuits), and `CONNECTED` (stream established).

### Streams

A single circuit can carry multiple streams. When the proxy wants to reach a new destination (e.g. `example.com:80`), it opens a stream by sending a `BEGIN` relay to the exit router, which dials the actual TCP connection. Data then flows through `DATA` relays in both directions simultaneously using goroutines.

## Architecture

```
torgo/
├── router.go                  # Core router: circuit management, cell routing, proxy server
├── proxy/                     # HTTP request parsing and header rewriting
├── regagent/                  # UDP registration agent for peer discovery
├── concurrent_circuitmap/     # Sharded concurrent map (circuit → channel)
├── concurrent_uint16map/      # Sharded concurrent map (stream ID → channel)
├── concurrent_uint32map/      # Sharded concurrent map (router ID → channel)
└── run                        # Launch script
```

**Router** (`router.go`) is the main component. It accepts TCP connections from other routers, builds and extends circuits, routes cells between circuits using forwarding tables, manages streams, and runs the HTTP proxy for local clients.

**Registration agent** (`regagent/`) handles peer discovery via a UDP protocol. Routers register themselves on startup and periodically re-register before their TTL expires. The proxy fetches available routers from this service when building circuits.

**Proxy** (`proxy/`) parses HTTP requests from the browser, extracts the destination address, and forwards traffic through the Tor circuit. Handles both HTTP and HTTPS tunneling.

**Concurrent maps** are sharded (32 shards each, FNV-32 hashing) to reduce lock contention across goroutines. Go didn't have generics at the time, so there are three type-specific implementations for circuit routing, stream management, and router connections.

## Running

```bash
./run <group> <instance> <proxy_port> <registration_server:port>
```

For example, to start a router in group 1, instance 1, with the proxy on port 8080:

```bash
./run 1 1 8080 registration.example.com:9000
```

Each router registers as `Tor61Router-<group>-<instance>` and derives its router ID from the group and instance numbers. Configure your browser's HTTP proxy to `localhost:<proxy_port>`.

## Limitations

This is an educational project. It implements Tor's routing architecture but not its cryptography. Data travels in plaintext through the circuit (real Tor uses layered AES encryption at each hop). There is also no integrity checking, no circuit pooling, and no directory authority.
