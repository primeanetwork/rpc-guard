# Primea RPC Guard

🛡️ Hardened JSON-RPC middleware for Ethereum-based chains.

## Features

- ✅ Blocks underpriced `eth_sendRawTransaction` (configurable gas floor)
- ✅ IP-based rate limiting per RPC method
- ✅ `eth_getLogs` block range limiter
- ✅ Hot-reloadable `config.json` without restart
- ✅ Prometheus metrics (`/metrics` endpoint)

## Usage

1. **Build:**

```bash
go build -o rpc-guard main.go
```

2. **Config:**

Create a `config.json` file:

```json
{
  "geth_rpc": "http://localhost:18545",
  "min_gas_price_gwei": 50,
  "log_block_range_limit": 50000,
  "rate_limits": {
    "eth_call": {
      "rate_per_sec": 1,
      "burst": 5
    },
    "eth_getLogs": {
      "rate_per_sec": 0.5,
      "burst": 3
    }
  }
}
```

3. **Run:**

```bash
./rpc-guard
```

4. **Prometheus:**

Access metrics at `http://localhost:8545/metrics`

## Systemd (optional)

```ini
[Unit]
Description=Primea RPC Guard
After=network.target

[Service]
ExecStart=/opt/primea/rpc-guard/rpc-guard
WorkingDirectory=/opt/primea/rpc-guard
Restart=always
User=superuser

[Install]
WantedBy=multi-user.target
```

Then:
```bash
sudo systemctl daemon-reexec
sudo systemctl enable rpc-guard
sudo systemctl start rpc-guard
```

---

Built for Primea Network – hardened EVM infra for real-world asset tokenization.
