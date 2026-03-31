# ton-payment-loadtest

Load test template for TON payment gateways, written in Go.

Simulates N real users paying concurrently:
1. Creates N orders in parallel via the gateway's HTTP API
2. Sends a real on-chain TON transaction per order (invoice_id in memo)
3. Polls the status endpoint every second until `confirmed` or 30s timeout

## Why Go + tonutils-go?

Standard load test tools (k6, Locust) only drive HTTP. Here we need real on-chain
transactions — Go has first-class TON tooling via [tonutils-go](https://github.com/xssnick/tonutils-go).

## Why one wallet per order?

TON wallets use a sequential `seqno`. Sending 100 transactions in parallel from
one wallet causes seqno conflicts — only the first tx lands, the rest are rejected.

Options:
- **Highload Wallet V3** — designed for parallel sends, uses `query_id` instead of `seqno`
- **N separate wallets** — one wallet per order; no conflicts, and better models real traffic (each user pays from their own wallet) ✓ *(this repo)*

## Setup

### 1. Configure

```bash
cp .env.example .env
# edit .env — set GATEWAY_BASE_URL, GATEWAY_API_KEY, GATEWAY_TON_ADDR
```

### 2. Adapt the API layer

Edit `main.go` and look for `// TODO: adapt` comments:
- `createOrderReq` — request body for your create-order endpoint
- `createOrderData` — fields from the response you need (invoice_id, ton_amount)
- `createOrder()` — endpoint path, auth header
- `getInvoiceStatus()` — status endpoint path
- Terminal statuses in the polling loop (`"confirmed"`, `"expired"`, `"failed"`)

### 3. Generate wallets

```bash
go run ./gen-wallets/
# flags: -n 100 -out wallets.json [-testnet]
```

> ⚠️ `wallets.json` contains mnemonics — treat it as a secret, never commit it.

### 4. Fund wallets

```bash
MAIN_WALLET_MNEMONIC="word1 ... word24" FUND_AMOUNT=0.01 go run ./fund-wallets/
```

`0.01 TON` per wallet covers:
- First-ever send: W5 contract deploy ≈ 0.005 TON
- Payment amount ≈ 0.0002 TON
- Network gas

### 5. Run

```bash
# Smoke test — 1 order, wallet #1
N=1 WALLET_INDEX=1 go run .

# Full load test — 100 parallel orders
go run .
```

Per-order logs → `./logs/<wallet_index>.log`  
Summary → stdout (JSON)

## Environment variables

| Variable | Required | Default | Description |
|---|---|---|---|
| `GATEWAY_BASE_URL` | ✓ | — | Gateway base URL |
| `GATEWAY_API_KEY` | ✓ | — | API key |
| `GATEWAY_TON_ADDR` | ✓ | — | Gateway TON wallet address (payment destination) |
| `N` | | `100` | Number of parallel orders |
| `ORDER_AMOUNT` | | `1` | Fiat amount per order |
| `ORDER_CURRENCY` | | `USD` | Fiat currency |
| `WALLET_INDEX` | | — | Pin all orders to one wallet (smoke test) |
| `TON_TESTNET` | | `0` | Use TON testnet |

**fund-wallets:**

| Variable | Required | Default | Description |
|---|---|---|---|
| `MAIN_WALLET_MNEMONIC` | ✓ | — | 24-word seed of the bank wallet |
| `FUND_AMOUNT` | | `0.1` | TON per wallet |
| `WALLET_VERSION` | | `W5` | Bank wallet version (`V3R2` `V4R2` `W5`) |

## Project structure

```
.
├── main.go               # load test runner
├── gen-wallets/main.go   # generate N W5 wallets → wallets.json
├── fund-wallets/main.go  # top-up wallets from a bank wallet
├── go.mod / go.sum
├── .env.example
└── wallets.json          # gitignored — never commit
```
