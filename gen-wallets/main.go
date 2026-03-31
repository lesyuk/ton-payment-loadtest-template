// gen-wallets generates N new TON W5 wallets and saves their mnemonics
// and addresses to a JSON file.
//
// The output file is sensitive — treat it like a secrets file.
//
// Usage:
//
//	go run ./gen-wallets/
//	go run ./gen-wallets/ -n 50 -out wallets.json -testnet
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"

	"github.com/xssnick/tonutils-go/liteclient"
	"github.com/xssnick/tonutils-go/ton"
	"github.com/xssnick/tonutils-go/ton/wallet"
)

// WalletRecord is shared with fund-wallets and the load test.
type WalletRecord struct {
	Index    int      `json:"index"`
	Address  string   `json:"address"`
	Mnemonic []string `json:"mnemonic"`
}

func main() {
	n := flag.Int("n", 100, "number of wallets to generate")
	out := flag.String("out", "wallets.json", "output file")
	testnet := flag.Bool("testnet", false, "use testnet liteservers")
	flag.Parse()

	ctx := context.Background()

	cfgURL := "https://ton.org/global.config.json"
	if *testnet {
		cfgURL = "https://ton-blockchain.github.io/testnet-global.config.json"
	}

	pool := liteclient.NewConnectionPool()
	if err := pool.AddConnectionsFromConfigUrl(ctx, cfgURL); err != nil {
		log.Fatalf("liteserver config: %v", err)
	}
	api := ton.NewAPIClient(pool)

	// W5 (V5R1Final) is the current default in most modern TON wallets.
	// NetworkGlobalID: -239 = mainnet, -3 = testnet.
	netID := int32(-239)
	if *testnet {
		netID = -3
	}
	walletVersion := wallet.ConfigV5R1Final{NetworkGlobalID: netID}

	records := make([]WalletRecord, 0, *n)
	for i := 1; i <= *n; i++ {
		words := wallet.NewSeed()
		w, err := wallet.FromSeed(api, words, walletVersion)
		if err != nil {
			log.Fatalf("wallet %d: %v", i, err)
		}
		addr := w.Address().String()
		records = append(records, WalletRecord{
			Index:    i,
			Address:  addr,
			Mnemonic: words,
		})
		fmt.Printf("[%d/%d] %s\n", i, *n, addr)
	}

	f, err := os.Create(*out)
	if err != nil {
		log.Fatalf("create %s: %v", *out, err)
	}
	defer f.Close()

	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	if err := enc.Encode(records); err != nil {
		log.Fatalf("encode: %v", err)
	}

	fmt.Printf("\n✓ Saved %d wallets to %s\n", len(records), *out)
	fmt.Println("Next step: fund them with  go run ./fund-wallets/")
}
