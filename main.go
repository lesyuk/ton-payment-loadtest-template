// ton-payment-loadtest-template — load test template for any TON payment gateway.
//
// The test simulates N real users paying concurrently:
//   1. Creates N orders via POST /your/create-order endpoint
//   2. Sends a real TON on-chain transaction from a dedicated wallet per order
//      (invoice_id in memo so the gateway can match the tx to the order)
//   3. Polls the status endpoint every second for up to 30s until "confirmed"
//
// Prerequisites (run once before the load test):
//
//	go run ./gen-wallets/          # generate N wallets → wallets.json
//	MAIN_WALLET_MNEMONIC="word1 ... word24" FUND_AMOUNT=0.01 go run ./fund-wallets/
//
// Smoke test (single order, wallet index 1):
//
//	N=1 WALLET_INDEX=1 go run .
//
// Full load test:
//
//	go run .
//
// Per-order logs → ./logs/<wallet_index>.log
// Stdout → high-level summary only
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/xssnick/tonutils-go/address"
	"github.com/xssnick/tonutils-go/liteclient"
	"github.com/xssnick/tonutils-go/tlb"
	"github.com/xssnick/tonutils-go/ton"
	"github.com/xssnick/tonutils-go/ton/wallet"
	"github.com/xssnick/tonutils-go/tvm/cell"
)

// ─── Config ───────────────────────────────────────────────────────────────────
//
// All settings are read from environment variables so nothing sensitive is
// hardcoded. Copy .env.example → .env and fill in the values.

func envOrDie(key string) string {
	v := os.Getenv(key)
	if v == "" {
		fmt.Fprintf(os.Stderr, "required env var %s is not set\n", key)
		os.Exit(1)
	}
	return v
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

const (
	defaultN      = 100
	pollTimeout   = 30 * time.Second
	pollInterval  = 1 * time.Second
	createWorkers = 100
	walletsFile   = "wallets.json"
	logsDir       = "logs"
)

// ─── Domain types ─────────────────────────────────────────────────────────────

type WalletRecord struct {
	Index    int      `json:"index"`
	Address  string   `json:"address"`
	Mnemonic []string `json:"mnemonic"`
}

type orderEntry struct {
	orderID   string
	invoiceID string // returned by the gateway; used as TON tx memo
	tonAmount string // exact amount to send on-chain
	log       *slog.Logger
	logFile   *os.File
}

// ─── Gateway API ──────────────────────────────────────────────────────────────
//
// TODO: adapt the request/response structs and HTTP calls to match your gateway's API.

// createOrderReq is the body sent to POST /your-endpoint/create-order.
// TODO: rename fields to match your API.
type createOrderReq struct {
	OrderID    string `json:"order_id"`
	Amount     string `json:"amount"`
	Currency   string `json:"currency"`
	WebhookURL string `json:"webhook_url,omitempty"`
}

// createOrderData is the relevant part of the successful create-order response.
// TODO: rename fields to match your API.
type createOrderData struct {
	InvoiceID string `json:"invoice_id"` // unique ID — used as TON tx memo
	TonAmount string `json:"ton_amount"` // exact amount the user must send
}

type createOrderResp struct {
	Data  *createOrderData `json:"data"`
	Error interface{}      `json:"error"`
}

type statusData struct {
	Status string `json:"status"` // e.g. "pending" | "confirmed" | "expired"
}

type statusResp struct {
	Data *statusData `json:"data"`
}

var httpClient = &http.Client{Timeout: 15 * time.Second}

func createOrder(ctx context.Context, baseURL, apiKey string, perLog *slog.Logger) (*orderEntry, error) {
	orderID := "load-" + uuid.New().String()

	// TODO: adapt the request body to your gateway's schema.
	body, _ := json.Marshal(createOrderReq{
		OrderID:  orderID,
		Amount:   envOr("ORDER_AMOUNT", "1"),
		Currency: envOr("ORDER_CURRENCY", "USD"),
	})

	// TODO: adapt the endpoint path to your gateway's API.
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/api/payments/orders", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	// TODO: adapt auth header name if needed (X-Api-Key, Authorization, etc.)
	req.Header.Set("X-Api-Key", apiKey)

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	rawBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		snippet := string(rawBody)
		if len(snippet) > 300 {
			snippet = snippet[:300]
		}
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, snippet)
	}

	var out createOrderResp
	if err := json.Unmarshal(rawBody, &out); err != nil {
		snippet := string(rawBody)
		if len(snippet) > 300 {
			snippet = snippet[:300]
		}
		return nil, fmt.Errorf("decode: %w — body: %s", err, snippet)
	}
	if out.Data == nil {
		return nil, fmt.Errorf("nil data, HTTP %d", resp.StatusCode)
	}

	e := &orderEntry{
		orderID:   orderID,
		invoiceID: out.Data.InvoiceID,
		tonAmount: out.Data.TonAmount,
		log:       perLog,
	}
	perLog.Info("order created", "order_id", e.orderID, "invoice_id", e.invoiceID, "ton_amount", e.tonAmount)
	return e, nil
}

// TODO: adapt endpoint path and auth.
func getInvoiceStatus(ctx context.Context, baseURL, apiKey, invoiceID string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		fmt.Sprintf("%s/api/payments/%s/status", baseURL, invoiceID), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("X-Api-Key", apiKey)

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	var out statusResp
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("decode: %w", err)
	}
	if out.Data == nil {
		return "", fmt.Errorf("nil data, HTTP %d", resp.StatusCode)
	}
	return out.Data.Status, nil
}

// ─── TON ──────────────────────────────────────────────────────────────────────

func initTONAPI(ctx context.Context) (*ton.APIClient, error) {
	cfgURL := "https://ton.org/global.config.json"
	if os.Getenv("TON_TESTNET") == "1" {
		cfgURL = "https://ton-blockchain.github.io/testnet-global.config.json"
	}
	pool := liteclient.NewConnectionPool()
	if err := pool.AddConnectionsFromConfigUrl(ctx, cfgURL); err != nil {
		return nil, fmt.Errorf("liteserver config: %w", err)
	}
	return ton.NewAPIClient(pool), nil
}

func walletFromRecord(api *ton.APIClient, rec WalletRecord) (*wallet.Wallet, error) {
	// W5 (V5R1Final) — generated by gen-wallets, mainnet.
	// Change NetworkGlobalID to -3 for testnet.
	return wallet.FromSeed(api, rec.Mnemonic, wallet.ConfigV5R1Final{NetworkGlobalID: -239})
}

// sendTONTx sends ton_amount TON to gatewayAddr with invoiceID as the memo.
// The gateway uses the memo to match the on-chain tx to the order.
// Retries up to 3 times on transient liteserver errors.
func sendTONTx(ctx context.Context, w *wallet.Wallet, e *orderEntry, gatewayAddr string) error {
	// Standard TON comment cell: op=0 (32-bit zero) + UTF-8 text.
	body := cell.BeginCell().
		MustStoreUInt(0, 32).
		MustStoreStringSnake(e.invoiceID). // invoice_id is the memo
		EndCell()

	msg := &wallet.Message{
		Mode: 3, // pay gas separately + ignore action-phase errors
		InternalMessage: &tlb.InternalMessage{
			IHRDisabled: true,
			Bounce:      false,
			DstAddr:     address.MustParseAddr(gatewayAddr),
			Amount:      tlb.MustFromTON(e.tonAmount),
			Body:        body,
		},
	}

	var err error
	for attempt := 1; attempt <= 3; attempt++ {
		// waitConfirmation=true handles StateInit for first-ever send from this wallet.
		err = w.Send(ctx, msg, true)
		if err == nil {
			return nil
		}
		e.log.Warn("TON tx attempt failed", "attempt", attempt, "err", err.Error())
	}
	return err
}

// ─── Helpers ──────────────────────────────────────────────────────────────────

func loadWallets() ([]WalletRecord, error) {
	f, err := os.Open(walletsFile)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var recs []WalletRecord
	return recs, json.NewDecoder(f).Decode(&recs)
}

func openOrderLog(name string) (*slog.Logger, *os.File) {
	f, err := os.Create(fmt.Sprintf("%s/%s.log", logsDir, name))
	if err != nil {
		return slog.New(slog.NewJSONHandler(io.Discard, nil)), nil
	}
	return slog.New(slog.NewJSONHandler(f, nil)), f
}

// ─── Main ─────────────────────────────────────────────────────────────────────

func main() {
	stdLog := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	ctx := context.Background()

	// Required env vars.
	baseURL := envOrDie("GATEWAY_BASE_URL") // e.g. https://your-gateway.example.com
	apiKey := envOrDie("GATEWAY_API_KEY")
	gatewayAddr := envOrDie("GATEWAY_TON_ADDR") // TON address of the gateway wallet

	// Optional.
	n := defaultN
	if v := os.Getenv("N"); v != "" {
		if _, err := fmt.Sscanf(v, "%d", &n); err != nil {
			stdLog.Error("invalid N", "value", v)
			os.Exit(1)
		}
	}
	walletIndex := 0 // 0 = use wallet[i] per order; >0 = pin all orders to one wallet
	if v := os.Getenv("WALLET_INDEX"); v != "" {
		if _, err := fmt.Sscanf(v, "%d", &walletIndex); err != nil || walletIndex < 1 {
			stdLog.Error("WALLET_INDEX must be >= 1", "value", v)
			os.Exit(1)
		}
	}

	if err := os.MkdirAll(logsDir, 0o755); err != nil {
		stdLog.Error("cannot create logs dir", "err", err.Error())
		os.Exit(1)
	}

	// Load wallets early — used for log file naming.
	walletsEarly, _ := loadWallets()

	// ── 1. Create N orders concurrently ──────────────────────────────────────

	stdLog.Info("creating orders", "n", n, "workers", createWorkers)

	jobs := make(chan int, n)
	for i := 1; i <= n; i++ {
		jobs <- i
	}
	close(jobs)

	var (
		mu      sync.Mutex
		entries []*orderEntry
		wg      sync.WaitGroup
	)

	for range createWorkers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				logName := fmt.Sprintf("%d", i)
				if len(walletsEarly) > 0 {
					logName = fmt.Sprintf("%d", walletsEarly[(i-1)%len(walletsEarly)].Index)
				}
				perLog, f := openOrderLog(logName)

				e, err := createOrder(ctx, baseURL, apiKey, perLog)
				if err != nil {
					perLog.Error("create order failed", "i", i, "err", err.Error())
					stdLog.Error("create order failed", "i", i, "err", err.Error())
					if f != nil {
						f.Close()
					}
					continue
				}
				e.logFile = f

				mu.Lock()
				entries = append(entries, e)
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	stdLog.Info("orders ready", "created", len(entries), "failed", n-len(entries))

	defer func() {
		for _, e := range entries {
			if e.logFile != nil {
				e.logFile.Close()
			}
		}
	}()

	// ── 2. Send TON transactions ──────────────────────────────────────────────
	//
	// Why one wallet per order?
	// TON wallets use a sequential seqno — parallel sends from one wallet cause
	// conflicts (only the first tx passes, the rest are rejected by the chain).
	// Using N separate wallets eliminates the problem and better represents real
	// production traffic (each user pays from their own wallet).
	//
	// Alternative: Highload Wallet V3 (uses query_id instead of seqno).
	// We chose the wallet-per-order approach as it's more realistic.

	wallets, walletsErr := walletsEarly, error(nil)
	if len(walletsEarly) == 0 {
		wallets, walletsErr = loadWallets()
	}

	switch {
	case walletsErr == nil && len(wallets) >= len(entries):
		stdLog.Info("mode: parallel (wallets.json)", "wallets", len(wallets))

		tonAPI, err := initTONAPI(ctx)
		if err != nil {
			stdLog.Error("TON API init failed", "err", err.Error())
			stdLog.Warn("skipping TON transactions")
			break
		}

		var pinnedWallet *WalletRecord
		if walletIndex > 0 {
			for i := range wallets {
				if wallets[i].Index == walletIndex {
					pinnedWallet = &wallets[i]
					break
				}
			}
			if pinnedWallet == nil {
				stdLog.Error("WALLET_INDEX not found", "index", walletIndex)
				os.Exit(1)
			}
		}

		var txWg sync.WaitGroup
		for i, e := range entries {
			rec := wallets[i%len(wallets)]
			if pinnedWallet != nil {
				rec = *pinnedWallet
			}
			txWg.Add(1)
			go func(e *orderEntry, rec WalletRecord) {
				defer txWg.Done()
				e.log.Info("sending TON tx",
					"wallet_index", rec.Index,
					"to", gatewayAddr,
					"amount", e.tonAmount,
					"memo", e.invoiceID,
				)
				w, err := walletFromRecord(tonAPI, rec)
				if err != nil {
					e.log.Error("wallet init failed", "err", err.Error())
					return
				}
				if err := sendTONTx(ctx, w, e, gatewayAddr); err != nil {
					e.log.Error("TON tx failed", "order_id", e.orderID, "err", err.Error())
					stdLog.Error("TON tx failed", "order_id", e.orderID, "err", err.Error())
				} else {
					e.log.Info("TON tx sent", "order_id", e.orderID, "invoice_id", e.invoiceID)
					stdLog.Info("TON tx sent", "order_id", e.orderID, "invoice_id", e.invoiceID)
				}
			}(e, rec)
		}
		txWg.Wait()
		stdLog.Info("all TON transactions done")

	case os.Getenv("WALLET_MNEMONIC") != "":
		// Sequential fallback: single wallet, 3s delay between txs to avoid seqno conflicts.
		stdLog.Warn("mode: sequential fallback (single WALLET_MNEMONIC)")
		tonAPI, err := initTONAPI(ctx)
		if err != nil {
			stdLog.Error("TON API init failed", "err", err.Error())
			break
		}
		w, err := wallet.FromSeed(tonAPI, strings.Fields(os.Getenv("WALLET_MNEMONIC")), wallet.V4R2)
		if err != nil {
			stdLog.Error("wallet init", "err", err.Error())
			break
		}
		for _, e := range entries {
			if err := sendTONTx(ctx, w, e, gatewayAddr); err != nil {
				stdLog.Error("TON tx failed", "order_id", e.orderID, "err", err.Error())
			} else {
				stdLog.Info("TON tx sent", "order_id", e.orderID)
			}
			time.Sleep(3 * time.Second)
		}

	default:
		stdLog.Warn("no TON wallet configured — skipping on-chain transactions",
			"hint", "run gen-wallets + fund-wallets, or set WALLET_MNEMONIC",
		)
	}

	// ── 3. Poll invoice statuses concurrently ────────────────────────────────

	stdLog.Info("polling started", "invoices", len(entries), "timeout", pollTimeout)

	pending := make(map[string]*orderEntry, len(entries))
	for _, e := range entries {
		pending[e.invoiceID] = e
	}

	var confirmed, terminal int
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	deadline := time.Now().Add(pollTimeout)

	type pollResult struct {
		invoiceID string
		status    string
		err       error
	}

	for len(pending) > 0 && time.Now().Before(deadline) {
		<-ticker.C

		results := make(chan pollResult, len(pending))
		var pollWg sync.WaitGroup
		for id := range pending {
			pollWg.Add(1)
			go func(id string) {
				defer pollWg.Done()
				status, err := getInvoiceStatus(ctx, baseURL, apiKey, id)
				results <- pollResult{id, status, err}
			}(id)
		}
		pollWg.Wait()
		close(results)

		for r := range results {
			e := pending[r.invoiceID]
			if r.err != nil {
				e.log.Error("poll error", "err", r.err.Error())
				continue
			}
			switch r.status {
			case "confirmed":
				e.log.Info("confirmed ✓", "order_id", e.orderID)
				stdLog.Info("confirmed ✓", "order_id", e.orderID, "invoice_id", e.invoiceID)
				delete(pending, r.invoiceID)
				confirmed++
			case "expired", "failed":
				// TODO: add other terminal statuses your gateway may return.
				e.log.Warn("terminal", "status", r.status)
				stdLog.Warn("terminal", "order_id", e.orderID, "status", r.status)
				delete(pending, r.invoiceID)
				terminal++
			default:
				e.log.Debug("pending", "status", r.status)
			}
		}
	}

	for _, e := range pending {
		e.log.Warn("timeout", "order_id", e.orderID, "invoice_id", e.invoiceID)
		stdLog.Warn("timeout", "order_id", e.orderID, "invoice_id", e.invoiceID)
	}

	stdLog.Info("done",
		"total", len(entries),
		"confirmed", confirmed,
		"terminal", terminal,
		"timeout", len(pending),
		"logs_dir", logsDir,
	)
}
