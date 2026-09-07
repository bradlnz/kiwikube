package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestNodeAndPodMetrics(t *testing.T) {
	for raw, want := range map[string]float64{"500m": .5, "25n": 25e-9, "2Gi": 2 << 30, "1e3": 1000, "0": 0, "invalid": 0, "NaN": 0, "+Inf": 0} {
		if got := quantity(raw); math.Abs(got-want) > 1e-12*max(1, math.Abs(want)) {
			t.Fatalf("quantity %s: %v", raw, got)
		}
	}
	var previous, current nodeSummary
	if err := json.Unmarshal([]byte(`{"node":{"nodeName":"node-a","cpu":{"time":"2026-09-07T00:00:02Z","usageNanoCores":2000000000},"memory":{"time":"2026-09-07T00:00:02Z","workingSetBytes":8388608},"fs":{"usedBytes":50,"capacityBytes":100},"network":{"time":"2026-09-07T00:00:02Z","name":"eth0","rxBytes":3000,"txBytes":6000}}}`), &current); err != nil {
		t.Fatal(err)
	}
	previous = current
	previous.Node.Network.Time = previous.Node.Network.Time.Add(-2 * time.Second)
	rx, tx := uint64(1000), uint64(2000)
	previous.Node.Network.RxBytes, previous.Node.Network.TxBytes = &rx, &tx
	node := record{metadata: metadata{Name: "node-a"}, Capacity: map[string]string{"cpu": "8", "memory": "16Mi"}}
	rxPeak, txPeak := float64(0), float64(0)
	stats := makeNodeStats(current, previous, node, &rxPeak, &txPeak)
	if stats.gauges[0].value != 2 || stats.gauges[1].limit != 16<<20 || stats.gauges[3].value != 1000 || stats.gauges[4].value != 2000 {
		t.Fatalf("bad metrics: %+v", stats.gauges)
	}
	if counterRate(&rx, current.Node.Network.RxBytes, current.Node.Network.Time, previous.Node.Network.Time) != -1 || counterRate(nil, &rx, current.Node.Network.Time, previous.Node.Network.Time) != -1 {
		t.Fatal("counter reset/missing metric became usage")
	}
	p := colors("kiwi")
	ansi := regexp.MustCompile(`\x1b\[[0-9;]*m`)
	for _, cols := range []int{20, 40, 110} {
		full := gaugeLine(100, 100, cols, p)
		if width(ansi.ReplaceAllString(full, "")) != cols || !strings.Contains(full, "\x1b[38;5;82m█") || !strings.Contains(full, "\x1b[38;5;196m█") {
			t.Fatal("gauge width or gradient")
		}
	}
	if !strings.Contains(gaugeLine(-1, 100, 40, p), "n/a") {
		t.Fatal("missing stats displayed as zero")
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/nodes/node-a/proxy/stats/summary":
			json.NewEncoder(w).Encode(current)
		case "/apis/metrics.k8s.io/v1beta1/namespaces/default/pods/api-1":
			fmt.Fprint(w, `{"metadata":{"name":"api-1","namespace":"default"},"timestamp":"2026-09-07T00:00:02Z","containers":[{"usage":{"cpu":"100m","memory":"1Mi"}},{"usage":{"cpu":"200m","memory":"2Mi"}}]}`)
		default:
			http.Error(w, "Forbidden", http.StatusForbidden)
		}
	}))
	defer server.Close()
	api := &kubeAPI{base: server.URL, client: server.Client()}
	pod := record{metadata: metadata{Name: "api-1", Namespace: "default"}, Limits: map[string]float64{"cpu": 1}}
	metrics, err := loadPodMetrics(context.Background(), api, pod, node)
	if err != nil || metrics.gauges[0].value < .299 || metrics.gauges[0].value > .301 || metrics.gauges[0].limit != 1 || metrics.gauges[1].limit != 16<<20 {
		t.Fatalf("pod metrics: %+v %v", metrics, err)
	}
	pod.Name = "denied"
	if _, err := loadPodMetrics(context.Background(), api, pod, node); err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatal("pod permission error hidden")
	}
	for _, name := range []string{"node-a", "denied"} {
		ctx, cancel := context.WithCancel(context.Background())
		results := make(chan viewResult, 1)
		done := make(chan struct{})
		node.Name = name
		go func() { defer close(done); pollNodeStats(ctx, api, node, time.Second, 9, results) }()
		select {
		case result := <-results:
			if result.generation != 9 || (name == "node-a" && result.stats == nil) || (name == "denied" && result.err == nil) {
				t.Fatal("node poll result")
			}
		case <-time.After(3 * time.Second):
			t.Fatal("node stats request timed out")
		}
		cancel()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("node poll did not cancel")
		}
	}
}
