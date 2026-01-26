# Conduit CLI

Command-line interface for running a Psiphon Conduit node - a volunteer-run proxy that relays traffic for users in censored regions.

## Quick Start

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

Contact Psiphon (info@psiphon.ca) to obtain valid configuration values.

## Usage

```bash
# Start with default settings
conduit start --psiphon-config ./psiphon_config.json

# Customize limits
conduit start --psiphon-config ./psiphon_config.json --max-clients 500 --bandwidth 10

# Verbose output (info messages)
conduit start --psiphon-config ./psiphon_config.json -v

# Debug output (everything)
conduit start --psiphon-config ./psiphon_config.json -vv
```

### Options

| Flag | Default | Description |
|------|---------|-------------|
| `--psiphon-config, -c` | - | Path to Psiphon network configuration file |
| `--max-clients, -m` | 200 | Maximum concurrent clients (1-1000) |
| `--bandwidth, -b` | 5 | Bandwidth limit per peer in Mbps (1-40) |
| `--data-dir, -d` | `./data` | Directory for keys and state |
| `-v` | - | Verbose output (use `-vv` for debug) |

## Multi-Instance Mode

For VPS and server deployments, use `--multi-instance` to automatically run multiple instances based on `--max-clients` (1 instance per 100 clients). Each instance gets its own cryptographic key and reputation with the Psiphon broker.

```bash
# Auto-split: 200 clients = 2 instances (100 each)
conduit start --psiphon-config ./psiphon_config.json --max-clients 200 --multi-instance

# 400 clients = 4 instances
conduit start -c ./psiphon_config.json -m 400 --multi-instance -v

# With stats files for monitoring
conduit start -c ./psiphon_config.json -m 300 --multi-instance --stats-file stats.json
```

### Multi-Instance Options

| Flag | Default | Description |
|------|---------|-------------|
| `--multi-instance` | false | Enable multi-instance mode |
| `--max-clients, -m` | 50 | Total clients (auto-split: 1 instance per 100) |
| `--bandwidth, -b` | 40 | Bandwidth limit per instance in Mbps |
| `--stats-file, -s` | - | Stats file pattern (creates per-instance files) |

Each instance creates a subdirectory named by its key hash (e.g., `a1b2c3d4/`) with its own key file.

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

## Docker

### Build with embedded config (recommended)

```bash
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

## Data Directory

Keys and state are stored in the data directory (default: `./data`):
- `conduit_key.json` - Node identity keypair (preserve this!)

The broker builds reputation for your proxy based on this key. If you lose it, you'll need to build reputation from scratch.

## License

GNU General Public License v3.0
