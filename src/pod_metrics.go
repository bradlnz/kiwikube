package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"
)

type podMetrics struct {
	gauges []nodeGauge
	at     time.Time
}
type podMetricResult struct {
	generation int
	metrics    *podMetrics
	err        error
}

func loadPodMetrics(ctx context.Context, api *kubeAPI, pod, node record) (*podMetrics, error) {
	body, err := api.get(ctx, "/apis/metrics.k8s.io/v1beta1/namespaces/"+url.PathEscape(pod.Namespace)+"/pods/"+url.PathEscape(pod.Name), nil)
	if err != nil {
		return nil, err
	}
	defer body.Close()
	var result struct {
		Metadata   metadata
		Timestamp  time.Time
		Containers []struct{ Usage map[string]string }
	}
	if err = json.NewDecoder(io.LimitReader(body, 4<<20)).Decode(&result); err != nil {
		return nil, err
	}
	if result.Metadata.Name != pod.Name || result.Metadata.Namespace != pod.Namespace {
		return nil, fmt.Errorf("pod metrics response did not match the selected pod")
	}
	metrics := &podMetrics{at: result.Timestamp}
	for _, resource := range []string{"cpu", "memory"} {
		used := float64(0)
		if len(result.Containers) == 0 {
			used = -1
		}
		for _, container := range result.Containers {
			if _, ok := container.Usage[resource]; !ok {
				used = -1
				break
			}
			used += quantity(container.Usage[resource])
		}
		limit, basis := pod.Limits[resource], "limit"
		if limit <= 0 {
			limit, basis = quantity(node.Capacity[resource]), "node capacity"
		}
		label, detail := "CPU", "Unavailable"
		if resource == "memory" {
			label = "Memory"
		}
		if used >= 0 {
			if resource == "cpu" {
				detail = fmt.Sprintf("%.3f cores", used)
			} else {
				detail = byteLabel(used)
			}
			if limit > 0 {
				total := byteLabel(limit)
				if resource == "cpu" {
					total = fmt.Sprintf("%.2f cores", limit)
				}
				detail += " / " + total
			} else {
				basis = "no capacity available"
			}
		}
		metrics.gauges = append(metrics.gauges, nodeGauge{label: label + " · " + basis, detail: detail, value: used, limit: limit})
	}
	return metrics, nil
}
func (a *app) podPanelWidth() int {
	if !a.showPodMetrics || a.resource != "pods" || a.view != nil {
		return 0
	}
	available := a.cols
	if side := a.sidebarWidth(); side > 0 {
		available -= side + 1
	}
	if available < 78 {
		return 0
	}
	return 34
}
func (a *app) podPanelLines(cols int, p palette) []string {
	var lines []string
	add := func(text, style string) { lines = append(lines, style+fit(" "+text, cols)) }
	pod, ok := a.selected()
	if !ok {
		add("Select a pod", p.muted)
		return lines
	}
	add(pod.Name, p.accent)
	add(pod.Namespace, p.base+a.namespaceColor(pod.Namespace))
	add("", p.base)
	if a.podMetrics == nil {
		message := first(a.podMetricsStatus, "Loading metrics…")
		// Errors can be longer than a single panel row.
		for width(message) > 0 {
			tail := dropCells(message, cols-2)
			add(strings.TrimSuffix(message, tail), p.muted)
			message = tail
		}
		return lines
	}
	for _, g := range a.podMetrics.gauges {
		add(g.label, p.accent)
		add(g.detail, p.base)
		lines = append(lines, gaugeLine(g.value, g.limit, cols, p))
		add("", p.base)
	}
	if !a.podMetrics.at.IsZero() {
		add("Sample "+a.podMetrics.at.Local().Format("15:04:05"), p.muted)
	}
	add("Node: "+first(pod.Node, "unscheduled"), p.muted)
	add("m / × hide metrics", p.muted)
	return lines
}
