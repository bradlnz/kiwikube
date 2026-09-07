package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type nodeSummary struct {
	Node struct {
		NodeName string
		CPU      struct {
			Time                                 time.Time
			UsageNanoCores, UsageCoreNanoSeconds *uint64
		}
		Memory struct {
			Time            time.Time
			WorkingSetBytes *uint64
		}
		Network struct {
			Time             time.Time
			Name             string
			RxBytes, TxBytes *uint64
		}
		Fs struct{ UsedBytes, CapacityBytes *uint64 }
	}
}
type nodeGauge struct {
	label, detail string
	value, limit  float64 // A negative value means the metric is unavailable.
}
type nodeStats struct {
	gauges []nodeGauge
	at     time.Time
}

func quantity(value string) float64 {
	if n, err := strconv.ParseFloat(value, 64); err == nil && !math.IsInf(n, 0) && !math.IsNaN(n) && n >= 0 {
		return n
	}
	for _, unit := range []struct {
		suffix string
		scale  float64
	}{{"Ki", 1 << 10}, {"Mi", 1 << 20}, {"Gi", 1 << 30}, {"Ti", 1 << 40}, {"Pi", 1 << 50}, {"Ei", 1 << 60}, {"n", 1e-9}, {"u", 1e-6}, {"m", 1e-3}, {"k", 1e3}, {"K", 1e3}, {"M", 1e6}, {"G", 1e9}, {"T", 1e12}, {"P", 1e15}, {"E", 1e18}} {
		if raw, ok := strings.CutSuffix(value, unit.suffix); ok {
			n, err := strconv.ParseFloat(raw, 64)
			n *= unit.scale
			if err == nil && !math.IsInf(n, 0) && !math.IsNaN(n) && n >= 0 {
				return n
			}
		}
	}
	return 0
}
func counterRate(current, previous *uint64, at, before time.Time) float64 {
	if current == nil || previous == nil || at.IsZero() || before.IsZero() || !at.After(before) || *current < *previous {
		return -1
	}
	return float64(*current-*previous) / at.Sub(before).Seconds()
}
func byteLabel(value float64) string {
	units := []string{"B", "KiB", "MiB", "GiB", "TiB"}
	i := 0
	for value >= 1024 && i < len(units)-1 {
		value /= 1024
		i++
	}
	return fmt.Sprintf("%.1f %s", value, units[i])
}
func metricValue(value *uint64) float64 {
	if value == nil {
		return -1
	}
	return float64(*value)
}
func makeNodeStats(current, previous nodeSummary, node record, rxPeak, txPeak *float64) *nodeStats {
	n, old := current.Node, previous.Node
	cpu := metricValue(n.CPU.UsageNanoCores)
	if cpu < 0 {
		cpu = counterRate(n.CPU.UsageCoreNanoSeconds, old.CPU.UsageCoreNanoSeconds, n.CPU.Time, old.CPU.Time)
	}
	if cpu >= 0 {
		cpu /= 1e9
	}
	memory, disk := metricValue(n.Memory.WorkingSetBytes), metricValue(n.Fs.UsedBytes)
	rx, tx := float64(-1), float64(-1)
	if n.Network.Name == old.Network.Name {
		rx = counterRate(n.Network.RxBytes, old.Network.RxBytes, n.Network.Time, old.Network.Time)
		tx = counterRate(n.Network.TxBytes, old.Network.TxBytes, n.Network.Time, old.Network.Time)
	}
	*rxPeak, *txPeak = max(*rxPeak, rx), max(*txPeak, tx)
	gauges := []nodeGauge{
		{label: "CPU", value: cpu, limit: quantity(node.Capacity["cpu"])},
		{label: "Memory", value: memory, limit: quantity(node.Capacity["memory"])},
		{label: "Disk", value: disk, limit: metricValue(n.Fs.CapacityBytes)},
		{label: "Receive · " + first(n.Network.Name, "network"), value: rx, limit: max(1, *rxPeak)},
		{label: "Transmit · " + first(n.Network.Name, "network"), value: tx, limit: max(1, *txPeak)},
	}
	for i := range gauges {
		g := &gauges[i]
		g.detail = "Unavailable"
		if i >= 3 {
			if g.value >= 0 {
				peak := *rxPeak
				if i == 4 {
					peak = *txPeak
				}
				g.detail = byteLabel(g.value) + "/s · observed peak " + byteLabel(peak) + "/s"
			} else if n.Network.RxBytes != nil && n.Network.TxBytes != nil {
				g.detail = "Waiting for a fresh counter sample"
			}
		} else if g.value >= 0 {
			if i == 0 {
				g.detail = fmt.Sprintf("%.2f / %.2f cores", g.value, g.limit)
			} else {
				g.detail = byteLabel(g.value) + " / " + byteLabel(max(0, g.limit))
			}
			if g.limit <= 0 {
				g.detail += " · capacity unavailable"
			}
		}
	}
	at := n.CPU.Time
	if n.Memory.Time.After(at) {
		at = n.Memory.Time
	}
	return &nodeStats{gauges: gauges, at: at}
}
func pollNodeStats(ctx context.Context, api *kubeAPI, node record, interval time.Duration, generation int, results chan<- viewResult) {
	var previous nodeSummary
	var lastStats *nodeStats
	var rxPeak, txPeak float64
	tick := time.NewTicker(max(time.Second, interval))
	defer tick.Stop()
	for {
		sample := nodeSummary{}
		request, stop := context.WithTimeout(ctx, 10*time.Second)
		body, err := api.get(request, "/api/v1/nodes/"+url.PathEscape(node.Name)+"/proxy/stats/summary", nil)
		if err == nil {
			err = json.NewDecoder(io.LimitReader(body, 16<<20)).Decode(&sample)
			body.Close()
			if err == nil && sample.Node.NodeName != node.Name {
				err = fmt.Errorf("node stats response did not match %s", node.Name)
			}
		}
		stop()
		result := viewResult{generation: generation, err: err}
		if err == nil {
			result.stats = makeNodeStats(sample, previous, node, &rxPeak, &txPeak)
			if lastStats != nil {
				if sample.Node.Network.Name == previous.Node.Network.Name && sample.Node.Network.Time.Equal(previous.Node.Network.Time) {
					copy(result.stats.gauges[3:], lastStats.gauges[3:])
				}
				if sample.Node.CPU.UsageNanoCores == nil && sample.Node.CPU.Time.Equal(previous.Node.CPU.Time) {
					result.stats.gauges[0] = lastStats.gauges[0]
				}
			}
			lastStats = result.stats
			// Kubelet may return cached timestamps; retain the baseline until a fresh sample.
			if sample.Node.Network.Time.After(previous.Node.Network.Time) {
				previous.Node.Network = sample.Node.Network
			}
			if sample.Node.CPU.Time.After(previous.Node.CPU.Time) {
				previous.Node.CPU = sample.Node.CPU
			}
		}
		select {
		case results <- result:
		case <-ctx.Done():
			return
		}
		select {
		case <-tick.C:
		case <-ctx.Done():
			return
		}
	}
}
func gaugeLine(value, limit float64, cols int, p palette) string {
	cells := max(1, cols-9)
	percent, filled := float64(0), 0
	if value >= 0 && limit > 0 {
		percent = value / limit * 100
		filled = int(math.Round(min(100, percent) / 100 * float64(cells)))
	}
	var out strings.Builder
	out.WriteString(p.base + " ")
	for i := 0; i < cells; i++ {
		position := float64(i) / float64(max(1, cells-1))
		color := 82
		if position >= .85 {
			color = 196
		} else if position >= .65 {
			color = 208
		} else if position >= .45 {
			color = 226
		}
		if i < filled {
			fmt.Fprintf(&out, "\x1b[38;5;%dm█", color)
		} else {
			out.WriteString(p.muted + "░")
		}
	}
	label := "   n/a"
	if value >= 0 && limit > 0 {
		label = fmt.Sprintf("%5.1f%%", min(999, percent))
	}
	out.WriteString(p.base + fit(label, cols-cells-1))
	return out.String()
}
func (v *viewer) nodeGaugeLines(cols int, p palette) []string {
	var lines []string
	if v.stats == nil {
		return []string{p.muted + fit(" "+first(v.status, "Loading node stats…"), cols)}
	}
	for _, gauge := range v.stats.gauges {
		if v.query != "" && !strings.Contains(strings.ToLower(gauge.label+" "+gauge.detail), strings.ToLower(v.query)) {
			continue
		}
		lines = append(lines, p.accent+"\x1b[1m"+fit(" "+gauge.label, cols)+"\x1b[22m", p.base+fit(" "+gauge.detail, cols), gaugeLine(gauge.value, gauge.limit, cols, p), p.base+fit("", cols))
	}
	if len(lines) == 0 {
		lines = append(lines, p.muted+fit(" No matching metrics", cols))
	}
	return lines
}

func (v *viewer) nodeStatsLines(cols int, p palette) []string {
	details := v.details
	if len(details) == 0 {
		details = []string{v.detailStatus}
	}
	if v.detailStatus != "Description" && len(v.details) > 0 {
		details = append([]string{v.detailStatus}, details...)
	}
	if cols < 90 {
		lines := v.nodeGaugeLines(cols, p)
		return append(lines, prettyDescribe(details, v.query, cols, p)...)
	}
	gaugeWidth := 42
	left := v.nodeGaugeLines(gaugeWidth, p)
	right := prettyDescribe(details, v.query, cols-gaugeWidth-1, p)
	var lines []string
	for i := 0; i < max(len(left), len(right)); i++ {
		l, r := p.base+fit("", gaugeWidth), p.base+fit("", cols-gaugeWidth-1)
		if i < len(left) {
			l = left[i]
		}
		if i < len(right) {
			r = right[i]
		}
		lines = append(lines, l+p.muted+"│"+r)
	}
	return lines
}
