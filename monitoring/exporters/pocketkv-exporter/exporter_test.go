package main

import (
	"bufio"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

func bulk(s string) []byte {
	return []byte(fmt.Sprintf("$%d\r\n%s\r\n", len(s), s))
}

type fakePocketKV struct {
	ln   net.Listener
	info string
	mem  string

	mu   sync.Mutex
	cmds [][]string
}

func newFakePocketKV(t *testing.T, info, mem string) *fakePocketKV {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	f := &fakePocketKV{ln: ln, info: info, mem: mem}
	go f.serve()
	return f
}

func (f *fakePocketKV) serve() {
	for {
		conn, err := f.ln.Accept()
		if err != nil {
			return
		}
		go func(c net.Conn) {
			defer c.Close()
			br := bufio.NewReader(c)
			for {
				v, err := readRESP(br)
				if err != nil {
					return
				}
				args := make([]string, 0, len(v.Arr))
				for _, a := range v.Arr {
					args = append(args, a.Str)
				}
				f.mu.Lock()
				f.cmds = append(f.cmds, args)
				f.mu.Unlock()
				if len(args) == 0 {
					return
				}
				switch strings.ToUpper(args[0]) {
				case "INFO":
					_, _ = c.Write(bulk(f.info))
				case "MEMSTAT":
					_, _ = c.Write(bulk(f.mem))
				default:
					_, _ = c.Write([]byte("-ERR unexpected command\r\n"))
				}
			}
		}(conn)
	}
}

func (f *fakePocketKV) addr() string { return f.ln.Addr().String() }

func (f *fakePocketKV) close() { _ = f.ln.Close() }

func sampleInfo() string {
	return "role:master\n" +
		"master_repl_offset:4096\n" +
		"slave_repl_applied_offset:2048\n" +
		"slave_repl_durable_offset:1024\n" +
		"repl_commit_gap:2\n" +
		"repl_commit_timeout:1\n" +
		"repl_commit_desync:0\n" +
		"repl_fullsync_count:3\n" +
		"repl_partialsync_ok_count:5\n" +
		"repl_partialsync_err_count:1\n" +
		"connected_slaves:2\n" +
		"replicas:2\n" +
		"rdma_fs_active_jobs:1\n" +
		"rdma_fs_sending_jobs:2\n" +
		"rdma_fs_waiting_jobs:3\n" +
		"rdma_fs_total_completed:4\n" +
		"rdma_fs_total_failed:1\n" +
		"rdma_fs_cq_errors:7\n" +
		"rdma_fs_cm_rejects:0\n" +
		"rdma_fs_resource_rejects:0\n" +
		"rdma_fs_connected_jobs:2\n" +
		"aof_submitted:100\n" +
		"aof_written:90\n" +
		"aof_fatal:1\n" +
		"aof_outstanding:2\n" +
		"aof_batch_count:10\n" +
		"aof_batch_us_total:5000\n" +
		"aof_error_count:1\n" +
		"aof_max_batch_age_us:2000\n" +
		"replica_min_ack_age_ms:2500\n" +
		"replica_slot_0_applied:1000\n" +
		"replica_slot_0_durable:900\n" +
		"replica_slot_0_ack_age_ms:1500\n" +
		"replica_slot_0_transport:3\n" +
		"ebpf_proxy_cache_bytes:555\n" +
		"master_host:127.0.0.1\n"
}

func sampleMemstat(classCount int) string {
	var b strings.Builder
	b.WriteString("backend=custom\nalloc_calls=10\nfree_calls=9\n")
	b.WriteString("current_allocated_bytes=1234\ncurrent_requested_bytes=1000\n")
	b.WriteString("internal_fragment_rate=0.123456\npage_utilization=0.500000\n")
	for i := 0; i < classCount; i++ {
		size := 8 * (i + 1) // 每个 class 的 size_class 标签必须唯一
		fmt.Fprintf(&b, "class_%d_size=%d\nclass_%d_pages=%d\nclass_%d_total_chunks=%d\nclass_%d_free_chunks=%d\nclass_%d_page_bytes=%d\n",
			i, size, i, i+1, i, 100+i, i, 10+i, i, 4096*(i+1))
	}
	return b.String()
}

func gather(t *testing.T, reg *prometheus.Registry) map[string]*dto.MetricFamily {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	out := make(map[string]*dto.MetricFamily, len(families))
	for _, f := range families {
		out[f.GetName()] = f
	}
	return out
}

func metricValue(t *testing.T, fams map[string]*dto.MetricFamily, name string, labels map[string]string) float64 {
	t.Helper()
	fam := fams[name]
	if fam == nil {
		t.Fatalf("metric %s not found", name)
	}
	for _, m := range fam.Metric {
		match := true
		for k, want := range labels {
			found := false
			for _, lp := range m.Label {
				if lp.GetName() == k && lp.GetValue() == want {
					found = true
					break
				}
			}
			if !found {
				match = false
				break
			}
		}
		if match {
			switch {
			case m.Gauge != nil:
				return m.Gauge.GetValue()
			case m.Counter != nil:
				return m.Counter.GetValue()
			case m.Untyped != nil:
				return m.Untyped.GetValue()
			}
		}
	}
	t.Fatalf("metric %s labels %v not found", name, labels)
	return 0
}

func TestExporterScrapeAndMapping(t *testing.T) {
	const classCount = 19
	fake := newFakePocketKV(t, sampleInfo(), sampleMemstat(classCount))
	defer fake.close()

	client := newRESPClient(fake.addr(), 2*time.Second)
	defer client.Close()
	exp := NewExporter(client, "test-sha", "auto")
	reg := prometheus.NewRegistry()
	reg.MustRegister(exp)

	fams := gather(t, reg)

	if v := metricValue(t, fams, "pocketkv_exporter_scrape_success", nil); v != 1 {
		t.Fatalf("scrape_success = %v, want 1", v)
	}
	if v := metricValue(t, fams, "pocketkv_allocator_allocated_bytes", nil); v != 1234 {
		t.Fatalf("allocated_bytes = %v", v)
	}
	if v := metricValue(t, fams, "pocketkv_allocator_fragmentation_ratio", nil); v != 0.123456 {
		t.Fatalf("fragmentation = %v", v)
	}
	if v := metricValue(t, fams, "pocketkv_replication_master_offset_bytes", nil); v != 4096 {
		t.Fatalf("master offset = %v", v)
	}
	if v := metricValue(t, fams, "pocketkv_commit_gap_total", nil); v != 2 {
		t.Fatalf("commit gap = %v", v)
	}
	if v := metricValue(t, fams, "pocketkv_aof_fatal", nil); v != 1 {
		t.Fatalf("aof_fatal = %v", v)
	}
	if v := metricValue(t, fams, "pocketkv_replication_min_ack_age_seconds", nil); v != 2.5 {
		t.Fatalf("replica ack age = %v", v)
	}
	if v := metricValue(t, fams, "pocketkv_aof_batch_count_total", nil); v != 10 {
		t.Fatalf("aof batch count = %v", v)
	}
	if v := metricValue(t, fams, "pocketkv_aof_batch_duration_seconds_total", nil); v != 0.005 {
		t.Fatalf("aof batch duration = %v", v)
	}
	if v := metricValue(t, fams, "pocketkv_aof_max_batch_age_seconds", nil); v != 0.002 {
		t.Fatalf("aof max batch age = %v", v)
	}
	if v := metricValue(t, fams, "pocketkv_replica_applied_offset_bytes", map[string]string{"slot": "0"}); v != 1000 {
		t.Fatalf("slot applied = %v", v)
	}
	if v := metricValue(t, fams, "pocketkv_replica_ack_age_seconds", map[string]string{"slot": "0"}); v != 1.5 {
		t.Fatalf("slot ack age = %v", v)
	}
	if v := metricValue(t, fams, "pocketkv_replica_transport_state",
		map[string]string{"slot": "0", "state": "ebpf"}); v != 1 {
		t.Fatalf("slot transport = %v", v)
	}
	for state, want := range map[string]float64{"active": 1, "sending": 2, "waiting": 3} {
		if v := metricValue(t, fams, "pocketkv_rdma_fullsync_jobs", map[string]string{"state": state}); v != want {
			t.Fatalf("rdma jobs %s = %v, want %v", state, v, want)
		}
	}
	// role=auto 时取 INFO role，且只允许 master/slave。
	if v := metricValue(t, fams, "pocketkv_build_info",
		map[string]string{"version": "test-sha", "role": "master"}); v != 1 {
		t.Fatalf("build_info = %v", v)
	}

	classFam := fams["pocketkv_allocator_class_chunks"]
	if classFam == nil {
		t.Fatal("class_chunks metric missing")
	}
	if got := len(classFam.Metric); got != classCount {
		t.Fatalf("class series = %d, want %d", got, classCount)
	}

	// 只允许两条只读命令，每次 scrape 各一次。
	fake.mu.Lock()
	cmds := append([][]string(nil), fake.cmds...)
	fake.mu.Unlock()
	if len(cmds) != 2 {
		t.Fatalf("commands = %v, want INFO+MEMSTAT", cmds)
	}
	if cmds[0][0] != "INFO" || cmds[1][0] != "MEMSTAT" {
		t.Fatalf("unexpected commands: %v", cmds)
	}

	// 第二次抓取沿用同一连接，仍然只有两条命令。
	fams = gather(t, reg)
	if v := metricValue(t, fams, "pocketkv_exporter_scrape_success", nil); v != 1 {
		t.Fatalf("second scrape_success = %v", v)
	}
}

func TestExporterScrapeFailureIsExported(t *testing.T) {
	fake := newFakePocketKV(t, sampleInfo(), sampleMemstat(19))
	addr := fake.addr()
	fake.close() // 目标不可达：Exporter 不得 panic，仍需暴露 scrape_success=0

	client := newRESPClient(addr, 200*time.Millisecond)
	defer client.Close()
	exp := NewExporter(client, "test-sha", "slave")
	reg := prometheus.NewRegistry()
	reg.MustRegister(exp)

	fams := gather(t, reg)
	if v := metricValue(t, fams, "pocketkv_exporter_scrape_success", nil); v != 0 {
		t.Fatalf("scrape_success = %v, want 0", v)
	}
	if v := metricValue(t, fams, "pocketkv_build_info",
		map[string]string{"version": "test-sha", "role": "slave"}); v != 1 {
		t.Fatalf("build_info = %v", v)
	}
}

func TestParseErrorsCounted(t *testing.T) {
	fake := newFakePocketKV(t, sampleInfo(), "alloc_calls=not-a-number\ncurrent_allocated_bytes=1\n")
	defer fake.close()
	client := newRESPClient(fake.addr(), time.Second)
	defer client.Close()
	exp := NewExporter(client, "test-sha", "master")
	reg := prometheus.NewRegistry()
	reg.MustRegister(exp)

	fams := gather(t, reg)
	if v := metricValue(t, fams, "pocketkv_exporter_parse_errors_total", nil); v < 1 {
		t.Fatalf("parse_errors_total = %v, want >=1", v)
	}
}
