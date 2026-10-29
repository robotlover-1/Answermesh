// benchf: filter Validate throughput/latency gRPC(50053) vs zrpc(50063).
// Temporary benchmark tool (not part of the service). Run from ai-chat-service:
//
//	GOFLAGS=-mod=mod CGO_ENABLED=1 go run ./cmd/benchf grpc 8 2000
//	GOFLAGS=-mod=mod CGO_ENABLED=1 go run ./cmd/benchf zrpc  8 2000
package main

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strconv"
	"sync"
	"time"

	"ai-chat-service/services/keywords-filter/proto"
	zrpc "echo-zrpc-go"
	"echo-zrpc-go/contract"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
)

var token = os.Getenv("AI_CHAT_KEYWORDS_ACCESS_TOKEN")

const text = "完全正常的句子，讨论天气"

func pct(a []float64, p float64) float64 { return a[int(p*float64(len(a)-1))] }
func avg(a []float64) float64 {
	var s float64
	for _, v := range a {
		s += v
	}
	return s / float64(len(a))
}

func report(name string, total int, el time.Duration, lats []float64) {
	sort.Float64s(lats)
	fmt.Printf("[%s] total=%d elapsed=%s qps=%.0f avg=%.0fus p50=%.0fus p95=%.0fus p99=%.0fus\n",
		name, total, el.Round(time.Millisecond), float64(total)/el.Seconds(),
		avg(lats), pct(lats, .50), pct(lats, .95), pct(lats, .99))
}

func benchGRPC(workers, total int) {
	conn, err := grpc.Dial("127.0.0.1:50053", grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		fmt.Println("dial err", err)
		return
	}
	defer conn.Close()
	per := total / workers
	start := time.Now()
	var mu sync.Mutex
	lats := make([]float64, 0, total)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c := proto.NewFilterClient(conn)
			ctx := metadata.NewOutgoingContext(context.Background(), metadata.Pairs("authorization", "Bearer "+token))
			for i := 0; i < per; i++ {
				t0 := time.Now()
				if _, err := c.Validate(ctx, &proto.FilterReq{Text: text}); err != nil {
					fmt.Println("grpc err", err)
					return
				}
				mu.Lock()
				lats = append(lats, float64(time.Since(t0).Microseconds()))
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	report("gRPC", total, time.Since(start), lats)
}

func benchZRPC(workers, total int) {
	per := total / workers
	start := time.Now()
	var mu sync.Mutex
	lats := make([]float64, 0, total)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cli, err := zrpc.NewClient(zrpc.ClientOptions{Host: "127.0.0.1", Port: 50063, Token: token})
			if err != nil {
				fmt.Println("zrpc newclient err", err)
				return
			}
			defer cli.Close()
			for i := 0; i < per; i++ {
				t0 := time.Now()
				var resp contract.ValidateResponse
				if err := cli.Unary(context.Background(), contract.MethodFilterValidate, &contract.FilterRequest{Text: text}, &resp); err != nil {
					fmt.Println("zrpc err", err)
					return
				}
				mu.Lock()
				lats = append(lats, float64(time.Since(t0).Microseconds()))
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	report("zrpc", total, time.Since(start), lats)
}

func main() {
	if len(os.Args) < 4 {
		fmt.Println("usage: benchf grpc|zrpc workers total")
		return
	}
	workers, _ := strconv.Atoi(os.Args[2])
	total, _ := strconv.Atoi(os.Args[3])
	switch os.Args[1] {
	case "grpc":
		benchGRPC(workers, total)
	case "zrpc":
		benchZRPC(workers, total)
	}
}
