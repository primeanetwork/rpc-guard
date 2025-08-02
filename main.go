package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// ===== CONFIG STRUCT =====

type RateLimitConfig struct {
	RatePerSec float64 `json:"rate_per_sec"`
	Burst      int     `json:"burst"`
}

type Config struct {
	GethRPC            string                     `json:"geth_rpc"`
	MinGasPriceGwei    int64                      `json:"min_gas_price_gwei"`
	LogBlockRangeLimit int64                      `json:"log_block_range_limit"`
	RateLimits         map[string]RateLimitConfig `json:"rate_limits"`
}

var (
	config     Config
	configLock sync.RWMutex
)

func loadConfig() {
	for {
		file, err := os.ReadFile("config.json")
		if err != nil {
			log.Fatalf("Failed to read config.json: %v", err)
		}
		var c Config
		if err := json.Unmarshal(file, &c); err != nil {
			log.Printf("⚠️ Config parse error: %v", err)
		} else {
			configLock.Lock()
			config = c
			configLock.Unlock()
		}
		time.Sleep(3 * time.Second)
	}
}

func getConfig() Config {
	configLock.RLock()
	defer configLock.RUnlock()
	return config
}

// ===== METRICS =====

var (
	rejects = prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "rpcguard_rejected_total", Help: "Rejected RPCs"},
		[]string{"method", "reason", "ip"},
	)
	accepts = prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "rpcguard_accepted_total", Help: "Accepted RPCs"},
		[]string{"method", "ip"},
	)
)

func init() {
	prometheus.MustRegister(rejects, accepts)
}

// ===== RATE LIMITING =====

type rateLimiter struct {
	tokens     float64
	last       time.Time
	ratePerSec float64
	burst      float64
	mutex      sync.Mutex
}

var ipLimiters = make(map[string]*rateLimiter)
var limiterLock sync.Mutex

func getLimiter(ip, method string, conf RateLimitConfig) *rateLimiter {
	key := ip + ":" + method
	limiterLock.Lock()
	defer limiterLock.Unlock()

	lim, ok := ipLimiters[key]
	if !ok {
		lim = &rateLimiter{
			tokens:     float64(conf.Burst),
			last:       time.Now(),
			ratePerSec: conf.RatePerSec,
			burst:      float64(conf.Burst),
		}
		ipLimiters[key] = lim
	}
	return lim
}

func (rl *rateLimiter) allow() bool {
	rl.mutex.Lock()
	defer rl.mutex.Unlock()

	now := time.Now()
	elapsed := now.Sub(rl.last).Seconds()
	rl.tokens = minF(rl.burst, rl.tokens+elapsed*rl.ratePerSec)
	rl.last = now

	if rl.tokens >= 1 {
		rl.tokens -= 1
		return true
	}
	return false
}

func minF(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}

// ===== RPC STRUCTS =====

type RPCRequest struct {
	JSONRPC string        `json:"jsonrpc"`
	Method  string        `json:"method"`
	Params  []interface{} `json:"params"`
	ID      interface{}   `json:"id"`
}

type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type RPCResponse struct {
	JSONRPC string      `json:"jsonrpc"`
	ID      interface{} `json:"id"`
	Error   *RPCError   `json:"error,omitempty"`
	Result  interface{} `json:"result,omitempty"`
}

// ===== MAIN ENTRY =====

func main() {
	go loadConfig()

	http.HandleFunc("/", handleRPC)
	http.Handle("/metrics", promhttp.Handler())

	log.Println("🛡️ Primea RPC Guard (with dynamic config) on :18545")
	log.Fatal(http.ListenAndServe(":18545", nil))
}

func handleRPC(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var req RPCRequest
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, "invalid JSON-RPC", 400)
		return
	}

	ip, _, _ := net.SplitHostPort(r.RemoteAddr)
	cfg := getConfig()

	// === Rate limiting per IP per method ===
	if limCfg, ok := cfg.RateLimits[req.Method]; ok {
		limiter := getLimiter(ip, req.Method, limCfg)
		if !limiter.allow() {
			rejectMetric(w, req.ID, req.Method, "rate_limited", ip, "Too many requests")
			return
		}
	}

	// === Special Handling ===
	switch req.Method {
	case "eth_sendRawTransaction":
		if len(req.Params) == 0 {
			rejectMetric(w, req.ID, req.Method, "no_param", ip, "Missing tx param")
			return
		}
		rawTxHex, _ := req.Params[0].(string)
		txBytes, _ := decodeHex(rawTxHex)
		var tx types.Transaction
		if err := rlp.DecodeBytes(txBytes, &tx); err == nil {
			minGas := big.NewInt(0).Mul(big.NewInt(cfg.MinGasPriceGwei), big.NewInt(1_000_000_000))
			if tx.GasPrice().Cmp(minGas) < 0 {
				rejectMetric(w, req.ID, req.Method, "low_gas_price", ip, "Gas price too low")
				return
			}
		}

	case "eth_getLogs":
		if len(req.Params) > 0 {
			filter, _ := req.Params[0].(map[string]interface{})
			from, to := blockNum(filter["fromBlock"]), blockNum(filter["toBlock"])
			if from != nil && to != nil && to.Sub(to, from).Cmp(big.NewInt(cfg.LogBlockRangeLimit)) > 0 {
				rejectMetric(w, req.ID, req.Method, "log_range", ip, "Log range too wide")
				return
			}
		}
	}

	// === Accept + forward ===
	accepts.WithLabelValues(req.Method, ip).Inc()
	resp, err := http.Post(cfg.GethRPC, "application/json", bytes.NewReader(body))
	if err != nil {
		http.Error(w, "upstream RPC failed", 502)
		return
	}
	defer resp.Body.Close()
	io.Copy(w, resp.Body)
}

func rejectMetric(w http.ResponseWriter, id interface{}, method, reason, ip, msg string) {
	rejects.WithLabelValues(method, reason, ip).Inc()
	json.NewEncoder(w).Encode(RPCResponse{
		JSONRPC: "2.0",
		ID:      id,
		Error: &RPCError{
			Code:    -32000,
			Message: msg,
		},
	})
}

func decodeHex(s string) ([]byte, error) {
	if strings.HasPrefix(s, "0x") {
		s = s[2:]
	}
	return new(big.Int).SetString(s, 16)
}

func blockNum(val interface{}) *big.Int {
	s, ok := val.(string)
	if !ok || !strings.HasPrefix(s, "0x") {
		return nil
	}
	n := new(big.Int)
	n.SetString(s[2:], 16)
	return n
}
