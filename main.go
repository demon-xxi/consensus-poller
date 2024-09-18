package main

import (
	"context"
	"fmt"
	"log"
	"math/big"
	"os"
	"os/signal"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/ethereum/go-ethereum/core/types"
	"github.com/lmittmann/w3"
	"github.com/lmittmann/w3/module/eth"
)

// Constants
const (
	rpcUrl       = "wss://base-sepolia-rpc.publicnode.com" // Replace with your RPC URL
	pollInterval = 1000 * time.Millisecond                 // Polling interva
	agentsCount  = 6                                       // Number of RPC client agents
)

// ANSI color codes for terminal output
const (
	ColorReset = "\033[0m"
	ColorRed   = "\033[31m"
	ColorGreen = "\033[32m"
)

// Update represents a block number update from a source
type Update struct {
	Source      string
	BlockNumber int64
}

// Collector holds the latest block numbers from each source
type Collector struct {
	mu           sync.Mutex
	latestBlocks map[string]int64
}

// NewCollector initializes a new Collector
func NewCollector() *Collector {
	return &Collector{
		latestBlocks: make(map[string]int64),
	}
}

// Helper functions for coloring text
func colorRed(s string) string {
	return ColorRed + s + ColorReset
}

func colorGreen(s string) string {
	return ColorGreen + s + ColorReset
}

// UpdateBlock updates the block number for a source and prints consensus delta
func (c *Collector) UpdateBlock(source string, blockNumber int64) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.latestBlocks[source] = blockNumber

	// Collect all block numbers
	var blockNumbers []int64
	for _, bn := range c.latestBlocks {
		blockNumbers = append(blockNumbers, bn)
	}

	if len(blockNumbers) == 0 {
		return
	}

	// Find min and max
	min, max := blockNumbers[0], blockNumbers[0]
	for _, bn := range blockNumbers[1:] {
		if bn < min {
			min = bn
		}
		if bn > max {
			max = bn
		}
	}

	delta := max - min

	// Extract and sort source names
	var sources []string
	for s := range c.latestBlocks {
		sources = append(sources, s)
	}
	sort.Strings(sources)

	// Prepare display with colors
	var parts []string
	for _, s := range sources {
		bn := c.latestBlocks[s]
		var colored string
		if bn == max {
			colored = colorGreen(fmt.Sprintf("%s:%d", s, bn))
		} else {
			colored = colorRed(fmt.Sprintf("%s:%d", s, bn))
		}
		parts = append(parts, colored)
	}
	display := fmt.Sprintf("Sources: [%s] | Consensus Delta: %d", strings.Join(parts, ", "), delta)

	fmt.Println(display)
}

// Starts an RPC client agent that subscribes to newHeads using w3
func startAgent(ctx context.Context, agentID int, rpcUrl string, collector *Collector, wg *sync.WaitGroup) {
	defer wg.Done()
	source := fmt.Sprintf("a-%d", agentID)

	// Create a new w3 client
	client, err := w3.Dial(rpcUrl)
	if err != nil {
		log.Printf("%s: WebSocket dial error: %v", source, err)
		return
	}
	defer client.Close()

	// Subscribe to newHeads
	headersChan := make(chan *types.Header)
	sub, err := client.Subscribe(eth.NewHeads(headersChan))
	if err != nil {
		log.Printf("%s: Subscription error: %v", source, err)
		return
	}
	defer sub.Unsubscribe()

	log.Printf("%s: Subscribed to newHeads", source)

	for {
		select {
		case <-ctx.Done():
			log.Printf("%s: Context canceled, exiting agent", source)
			return
		case head, ok := <-headersChan:
			if !ok {
				log.Printf("%s: Subscription channel closed", source)
				return
			}

			blockNumber := head.Number.Int64()

			collector.UpdateBlock(source, blockNumber)
		}
	}
}

// Starts a poller that periodically calls a specified JSON-RPC method using w3
func startPoller(ctx context.Context, pollerType string, rpcUrl string, pollInterval time.Duration, collector *Collector, wg *sync.WaitGroup) {
	defer wg.Done()
	source := pollerType

	// Convert WebSocket URL to HTTP URL
	httpRPCUrl := httpUrl(rpcUrl)

	// Create a new w3 client
	client, err := w3.Dial(httpRPCUrl)
	if err != nil {
		log.Printf("%s: HTTP dial error: %v", source, err)
		return
	}
	defer client.Close()

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Printf("%s: Context canceled, exiting poller", source)
			return
		case <-ticker.C:
			var blockNumber int64

			if pollerType == "eth_blockNumber" {
				// Call eth_blockNumber
				var res *big.Int
				err := client.Call(eth.BlockNumber().Returns(&res))
				if err != nil {
					log.Printf("%s: RPC error: %v", source, err)
					continue
				}
				blockNumber = res.Int64()
			} else if pollerType == "eth_getBlockByNumber" {
				// Call eth_getBlockByNumber("latest", false)
				var res *types.Header
				err := client.Call(eth.HeaderByNumber(nil).Returns(&res))
				if err != nil {
					log.Printf("%s: RPC error: %v", source, err)
					continue
				}
				blockNumber = res.Number.Int64()
			} else {
				log.Printf("Unknown poller type: %s", pollerType)
				continue
			}

			collector.UpdateBlock(source, blockNumber)
		}
	}
}

// Helper function to convert WebSocket URL to HTTP URL
func httpUrl(uri string) string {
	uri = strings.Replace(uri, "ws://", "http://", 1)
	uri = strings.Replace(uri, "wss://", "https://", 1)
	return uri
}

func main() {
	// Configure logger
	log.SetOutput(os.Stdout)
	log.SetFlags(log.LstdFlags | log.Lshortfile)

	// Create a context that is canceled on OS interrupt signals
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Listen for OS interrupt signals to gracefully shutdown
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)

	// Initialize collector
	collector := NewCollector()

	var wg sync.WaitGroup

	// Start agents
	for i := 1; i <= agentsCount; i++ {
		wg.Add(1)
		go startAgent(ctx, i, rpcUrl, collector, &wg)
	}

	// Start pollers
	wg.Add(1)
	go startPoller(ctx, "eth_blockNumber", rpcUrl, pollInterval, collector, &wg)
	wg.Add(1)
	go startPoller(ctx, "eth_getBlockByNumber", rpcUrl, pollInterval, collector, &wg)

	// Wait for interrupt signal
	<-sigs
	log.Println("Interrupt signal received. Shutting down...")
	cancel()

	// Wait for all goroutines to finish
	wg.Wait()
	log.Println("Shutdown complete.")
}
