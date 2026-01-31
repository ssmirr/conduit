# Conduit CLI

Command-line interface for running a Psiphon Conduit node - a volunteer-run proxy that relays traffic for users in censored regions.

## Quick Start

Want to run a Conduit station? Get the latest CLI release: https://github.com/Psiphon-Inc/conduit/releases

Our official CLI releases include an embedded psiphon config.

Contact Psiphon (conduit-oss@psiphon.ca) to discuss custom configuration values.

## Docker

Use the official Docker image, which includes an embedded Psiphon config. Docker Compose is a convenient way to run Conduit if you prefer a declarative setup.

```bash
docker compose up
```

The compose file enables Prometheus metrics on `:9090` inside the container. To scrape from the host, publish the port or run Prometheus on the same Docker network and scrape `conduit:9090`.

### Build from source with Docker

```bash
# Build with embedded config (recommended)
docker build -t conduit \
  --build-arg PSIPHON_CONFIG=psiphon_config.json \
  -f Dockerfile.embedded .
```

### Run with persistent data

**Important:** The Psiphon broker tracks proxy reputation by key. Always use a persistent volume to preserve your key across container restarts, otherwise you'll start with zero reputation and may not receive client connections.

```bash
# Using a named volume (recommended)
docker run -d --name conduit \
  -v conduit-data:/home/conduit/data \
  --restart unless-stopped \
  conduit

# Or using a host directory
mkdir -p /path/to/data && chown 1000:1000 /path/to/data
docker run -d --name conduit \
  -v /path/to/data:/home/conduit/data \
  --restart unless-stopped \
  conduit
```

### Build without embedded config

If you prefer to mount the config at runtime:

```bash
docker build -t conduit .

docker run -d --name conduit \
  -v conduit-data:/home/conduit/data \
  -v /path/to/psiphon_config.json:/config.json:ro \
  --restart unless-stopped \
  conduit start --psiphon-config /config.json
```

## Building From Source

```bash
# First time setup (clones required dependencies)
make setup

# Build
make build

# Run
./dist/conduit start --psiphon-config /path/to/psiphon_config.json
```

## Requirements

- **Go 1.24.x** (Go 1.25+ is not supported due to psiphon-tls compatibility)
- Psiphon network configuration file (JSON)

The Makefile will automatically install Go 1.24.3 if not present.

## Configuration

Conduit requires a Psiphon network configuration file containing connection parameters. See `psiphon_config.example.json` for the expected format.

Contact Psiphon (conduit-oss@psiphon.ca) to obtain valid configuration values.

## Usage

```bash
# Start with default settings
conduit start --psiphon-config ./psiphon_config.json

# Customize limits
conduit start --psiphon-config ./psiphon_config.json --max-clients 500 --bandwidth 10

# Enable Prometheus metrics
conduit start --psiphon-config ./psiphon_config.json --metrics-addr :9090

# Verbose output (info messages)
conduit start --psiphon-config ./psiphon_config.json -v

# Debug output (everything)
conduit start --psiphon-config ./psiphon_config.json -vv
```

### Options

| Flag | Default | Description |
|------|---------|-------------|
| `--psiphon-config, -c` | - | Path to Psiphon network configuration file |
| `--max-clients, -m` | 50 | Maximum concurrent clients |
| `--bandwidth, -b` | 40 | Bandwidth limit per peer in Mbps (-1 for unlimited) |
| `--data-dir, -d` | `./data` | Directory for keys and state |
| `--stats-file, -s` | - | Persist stats to JSON file |
| `--metrics-addr` | - | Prometheus metrics listen address (e.g., :9090) |
| `--geo` | false | Enable client geolocation tracking |
| `-v` | - | Verbose output (use `-vv` for debug) |

## Geo Stats

Track where your clients are connecting from:

```bash
conduit start --geo --stats-file stats.json --psiphon-config ./psiphon_config.json
```

On first run, the GeoLite2 database (~6MB) is automatically downloaded. Stats are updated in real-time as clients connect and disconnect.

Example `stats.json`:

```json
{
  "connectingClients": 5,
  "connectedClients": 12,
  "totalBytesUp": 1234567,
  "totalBytesDown": 9876543,
  "uptimeSeconds": 3600,
  "isLive": true,
  "geo": [
    {
      "code": "IR",
      "country": "Iran",
      "count": 3,
      "count_total": 47,
      "bytes_up": 524288000,
      "bytes_down": 2684354560
    },
    {
      "code": "CN",
      "country": "China",
      "count": 1,
      "count_total": 23,
      "bytes_up": 314572800,
      "bytes_down": 1610612736
    },
    {
      "code": "RELAY",
      "country": "Unknown (TURN Relay)",
      "count": 1,
      "count_total": 8,
      "bytes_up": 52428800,
      "bytes_down": 268435456
    }
  ],
  "timestamp": "2026-01-25T15:44:00Z"
}
```

| Field | Description |
|-------|-------------|
| `count` | Currently connected clients |
| `count_total` | Total unique clients since start |
| `bytes_up` | Total bytes uploaded since start |
| `bytes_down` | Total bytes downloaded since start |

**Notes:**
- Connections through TURN relay servers appear as `RELAY` since the actual client country cannot be determined.
- The `connectedClients` field is reported by the Psiphon broker and may differ slightly from the sum of geo `count` values, which are tracked locally via WebRTC callbacks.
- Bandwidth (`bytes_up`/`bytes_down`) is attributed to a country when the connection closes. Active connections contribute to `totalBytesUp`/`totalBytesDown` but won't appear in geo stats until they disconnect.

## Building

```bash
# Build for current platform
make build

# Build with embedded config (single-binary distribution)
make build-embedded PSIPHON_CONFIG=./psiphon_config.json

# Build for all platforms
make build-all

# Individual platform builds
make build-linux       # Linux amd64
make build-linux-arm   # Linux arm64
make build-darwin      # macOS Intel
make build-darwin-arm  # macOS Apple Silicon
make build-windows     # Windows amd64
```

Binaries are output to `dist/`.

## Data Directory

Keys and state are stored in the data directory (default: `./data`):

- `conduit_key.json` - Node identity keypair
  The Psiphon broker tracks proxy reputation by key. Always use a persistent volume to preserve your key across container restarts, otherwise you'll start with zero reputation and may not receive client connections for some time.

## License

GNU General Public License v3.0
