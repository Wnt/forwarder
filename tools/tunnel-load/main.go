// tunnel-load exercises cached public assets while multiplayer sockets stay open.
package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
)

func main() {
	base := flag.String("base", "https://turku.lab.madekivi.fi", "HTTP origin")
	pathsFile := flag.String("paths", "", "file of public asset paths, one per line")
	clients := flag.Int("clients", 10, "simultaneous players")
	parallel := flag.Int("parallel", 16, "parallel fetches per player")
	rounds := flag.Int("rounds", 5, "fetch each path this many times per player")
	timeout := flag.Duration("timeout", 5*time.Second, "per-fetch timeout including body")
	wsURL := flag.String("ws", "wss://turku.lab.madekivi.fi/", "multiplayer endpoint; empty disables")
	flag.Parse()
	paths := []string{"/coarse/health"}
	if *pathsFile != "" {
		f, err := os.Open(*pathsFile)
		must(err)
		paths = nil
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			if p := strings.TrimSpace(sc.Text()); p != "" {
				paths = append(paths, p)
			}
		}
		must(sc.Err())
		f.Close()
	}
	if len(paths) == 0 || *clients < 1 || *parallel < 1 || *rounds < 1 {
		panic("need paths and positive clients/parallel/rounds")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	var sockets []*websocket.Conn
	var socketErrors []string
	var wg sync.WaitGroup
	var mu sync.Mutex
	for i := 0; i < *clients && *wsURL != ""; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cctx, stop := context.WithTimeout(ctx, *timeout)
			defer stop()
			c, _, err := websocket.Dial(cctx, *wsURL, &websocket.DialOptions{
				HTTPHeader: http.Header{"Origin": {"https://wnt.github.io"}},
			})
			if err == nil {
				c.SetReadLimit(4 << 20)
				err = c.Write(cctx, websocket.MessageText, []byte(`{"v":2,"type":"hello"}`))
				if err == nil {
					_, body, readErr := c.Read(cctx)
					err = readErr
					var welcome struct{ Type string }
					if err == nil {
						err = json.Unmarshal(body, &welcome)
						if err == nil && welcome.Type != "welcome" {
							err = fmt.Errorf("expected welcome, got %q", welcome.Type)
						}
					}
				}
			}
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				socketErrors = append(socketErrors, err.Error())
				if c != nil {
					c.CloseNow()
				}
				return
			}
			sockets = append(sockets, c)
			// Drain broadcasts; never log the welcome's private session token.
			go func() {
				for {
					if _, _, err := c.Read(ctx); err != nil {
						return
					}
				}
			}()
		}()
	}
	wg.Wait()
	statuses := map[int]int{}
	errors := map[string]int{}
	validationErrors := map[string]int{}
	httpFailures := 0
	var elapsed []float64
	var transferred int64
	headerless := 0
	retryMissing := 0
	start := time.Now()
	for player := 0; player < *clients; player++ {
		tr := &http.Transport{MaxIdleConns: *parallel, MaxIdleConnsPerHost: *parallel, ForceAttemptHTTP2: true}
		defer tr.CloseIdleConnections()
		client := &http.Client{Transport: tr, Timeout: *timeout}
		jobs := make(chan string, len(paths)*(*rounds))
		for round := 0; round < *rounds; round++ {
			for _, p := range paths {
				jobs <- p
			}
		}
		close(jobs)
		for worker := 0; worker < *parallel; worker++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for requestPath := range jobs {
					req, err := http.NewRequestWithContext(ctx, "GET", strings.TrimRight(*base, "/")+requestPath, nil)
					must(err)
					req.Header.Set("Origin", "https://wnt.github.io")
					t0 := time.Now()
					resp, err := client.Do(req)
					var n int64
					validation := ""
					if err == nil {
						hash := sha256.New()
						var body bytes.Buffer
						var dst io.Writer = hash
						isJSON := strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json")
						if isJSON {
							dst = io.MultiWriter(hash, &body)
						}
						n, err = io.Copy(dst, resp.Body)
						resp.Body.Close()
						if err == nil && isJSON && !json.Valid(body.Bytes()) {
							validation = "invalid JSON response"
						}
						parts := strings.Split(path.Base(req.URL.Path), ".")
						if err == nil && resp.StatusCode == 200 && len(parts) >= 3 && len(parts[len(parts)-2]) == 64 {
							if hex.EncodeToString(hash.Sum(nil)) != parts[len(parts)-2] {
								validation = "asset SHA-256 mismatch"
							}
						}
					}
					ms := float64(time.Since(t0).Microseconds()) / 1000
					mu.Lock()
					if resp != nil {
						statuses[resp.StatusCode]++
						if resp.StatusCode >= 500 {
							httpFailures++
						}
						if resp.Header.Get("Access-Control-Allow-Origin") != "https://wnt.github.io" {
							headerless++
						}
						if resp.StatusCode >= 500 && resp.Header.Get("Retry-After") == "" {
							retryMissing++
						}
					}
					if err != nil {
						errors[err.Error()]++
					} else {
						elapsed = append(elapsed, ms)
						transferred += n
					}
					if validation != "" {
						validationErrors[validation]++
					}
					mu.Unlock()
				}
			}()
		}
	}
	wg.Wait()
	wsHealthy := 0
	for _, c := range sockets {
		pingCtx, stop := context.WithTimeout(ctx, *timeout)
		if err := c.Ping(pingCtx); err == nil {
			wsHealthy++
		} else {
			socketErrors = append(socketErrors, err.Error())
		}
		stop()
		c.CloseNow()
	}
	sort.Float64s(elapsed)
	pct := func(p float64) float64 {
		if len(elapsed) == 0 {
			return 0
		}
		return elapsed[int(float64(len(elapsed)-1)*p)]
	}
	failed := 0
	for _, n := range errors {
		failed += n
	}
	result := map[string]any{
		"base": *base, "clients": *clients, "parallel_per_client": *parallel,
		"requests": *clients * *rounds * len(paths), "statuses": statuses,
		"transport_failures": failed, "errors": errors, "headerless": headerless,
		"http_failures": httpFailures, "validation_errors": validationErrors,
		"retry_after_missing_5xx": retryMissing, "p50_ms": pct(.50), "p95_ms": pct(.95),
		"bytes": transferred, "seconds": time.Since(start).Seconds(),
		"ws_connected": len(sockets), "ws_healthy_at_end": wsHealthy, "ws_errors": socketErrors,
	}
	must(json.NewEncoder(os.Stdout).Encode(result))
	if failed+headerless+retryMissing+httpFailures > 0 || len(socketErrors)+len(validationErrors) > 0 {
		os.Exit(1)
	}
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}
