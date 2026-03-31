// fund-wallets reads wallets.json and sends FUND_AMOUNT TON to each wallet
// from a single "bank" wallet specified via MAIN_WALLET_MNEMONIC.
//
// Transactions are sent sequentially (seqno safety). For 100 wallets at
// ~5s per confirmed tx this takes roughly 8–10 minutes — run it once before
// the load test.
//
// Usage:
//
//	MAIN_WALLET_MNEMONIC="word1 ... word24" go run ./fund-wallets/
//	MAIN_WALLET_MNEMONIC="..." FUND_AMOUNT=0.05 WALLETS_FILE=wallets.json go run ./fund-wallets/
//	TON_TESTNET=1 MAIN_WALLET_MNEMONIC="..." go run ./fund-wallets/
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/xssnick/tonutils-go/address"
	"github.com/xssnick/tonutils-go/liteclient"
	"github.com/xssnick/tonutils-go/tlb"
	"github.com/xssnick/tonutils-go/ton"
	"github.com/xssnick/tonutils-go/ton/wallet"
)

type WalletRecord struct {
	Index    int      `json:"index"`
	Address  string   `json:"address"`
	Mnemonic []string `json:"mnemonic"`
}

func main() {
	ctx := context.Background()

	// ── Config from env ────────────────────────────────────────────────────

	mnemonic := os.Getenv("MAIN_WALLET_MNEMONIC")
	if mnemonic == "" {
		log.Fatal("MAIN_WALLET_MNEMONIC not set (space-separated 24-word seed phrase)")
	}

	fundAmount := "0.1" // TON per wallet
	if v := os.Getenv("FUND_AMOUNT"); v != "" {
		fundAmount = v
	}

	walletsFile := "wallets.json"
	if v := os.Getenv("WALLETS_FILE"); v != "" {
		walletsFile = v
	}

	cfgURL := "https://ton.org/global.config.json"
	if os.Getenv("TON_TESTNET") == "1" {
		cfgURL = "https://ton-blockchain.github.io/testnet-global.config.json"
	}

	// ── Load wallets ───────────────────────────────────────────────────────

	f, err := os.Open(walletsFile)
	if err != nil {
		log.Fatalf("open %s: %v\nRun gen-wallets first.", walletsFile, err)
	}
	defer f.Close()

	var records []WalletRecord
	if err := json.NewDecoder(f).Decode(&records); err != nil {
		log.Fatalf("decode wallets: %v", err)
	}
	fmt.Printf("Loaded %d wallets from %s\n", len(records), walletsFile)

	// ── Connect to TON ─────────────────────────────────────────────────────

	pool := liteclient.NewConnectionPool()
	if err := pool.AddConnectionsFromConfigUrl(ctx, cfgURL); err != nil {
		log.Fatalf("liteserver config: %v", err)
	}
	api := ton.NewAPIClient(pool)

	// ── Init main (bank) wallet ────────────────────────────────────────────
	// WALLET_VERSION is the version of the SENDER (bank wallet).
	// Recipient wallets (wallets.json) can be any version.
	// Supported: V3R1, V3R2, V4R1, V4R2, W5. Default: W5.

	walletVersions := map[string]wallet.VersionConfig{
		"V3R1": wallet.V3R1,
		"V3R2": wallet.V3R2,
		"V4R1": wallet.V4R1,
		"V4R2": wallet.V4R2,
		"W5":   wallet.ConfigV5R1Final{NetworkGlobalID: -239},
	}
	verName := os.Getenv("WALLET_VERSION")
	if verName == "" {
		verName = "W5"
	}
	ver, ok := walletVersions[verName]
	if !ok {
		log.Fatalf("unknown WALLET_VERSION %q, supported: V3R1 V3R2 V4R1 V4R2 W5", verName)
	}

	words := strings.Fields(mnemonic)
	mainWallet, err := wallet.FromSeed(api, words, ver)
	if err != nil {
		log.Fatalf("main wallet: %v", err)
	}
	fmt.Printf("Bank wallet : %s\n", mainWallet.Address().String())

	// Show current balance
	block, err := api.CurrentMasterchainInfo(ctx)
	if err == nil {
		acc, err := api.GetAccount(ctx, block, mainWallet.Address())
		if err == nil && acc.IsActive {
			fmt.Printf("Balance     : %s TON\n", acc.State.Balance.String())
		}
	}

	var fundAmountF float64
	fmt.Sscanf(fundAmount, "%f", &fundAmountF)
	fmt.Printf("Need approx : %.4f TON (+ gas)\n\n", float64(len(records))*fundAmountF)

	// ── Fund each wallet sequentially ──────────────────────────────────────

	fmt.Printf("Funding %d wallets with %s TON each...\n", len(records), fundAmount)

	success, failed := 0, 0
	for i, rec := range records {
		dst := address.MustParseAddr(rec.Address)

		// Fund — retry up to 3 times on send error.
		var sendErr error
		for attempt := 1; attempt <= 3; attempt++ {
			sendErr = mainWallet.Send(ctx, &wallet.Message{
				Mode: 3, // PayGasSeparately + IgnoreErrors
				InternalMessage: &tlb.InternalMessage{
					IHRDisabled: true,
					Bounce:      false,
					DstAddr:     dst,
					Amount:      tlb.MustFromTON(fundAmount),
				},
			}, true)
			if sendErr == nil {
				break
			}
			log.Printf("[%d/%d] send attempt %d/3 failed: %v", i+1, len(records), attempt, sendErr)
		}
		if sendErr != nil {
			log.Printf("[%d/%d] FAILED %s", i+1, len(records), rec.Address)
			failed++
		} else {
			fmt.Printf("[%d/%d] ✓ %s\n", i+1, len(records), rec.Address)
			success++
		}
	}

	fmt.Printf("\n✓ Funded: %d  ✗ Failed: %d\n", success, failed)
	if failed == 0 {
		fmt.Println("Next step: run the load test with  go run .")
	}
}
