package main

import (
	"fmt"
	"strings"
)

func (a *app) flowLines(cols int) []string {
	var lines []string
	services := map[string]record{}
	for _, service := range a.snapshot.Resources["services"] {
		services[service.Namespace+"/"+service.Name] = service
	}
	for _, kind := range []string{"ingresses", "services", "pods"} {
		if err := a.snapshot.Errors[kind]; err != "" {
			lines = append(lines, "Unavailable / stale "+kind+": "+err, "")
		}
	}
	for _, ingress := range a.snapshot.Resources["ingresses"] {
		if a.namespace != "" && ingress.Namespace != a.namespace {
			continue
		}
		for _, route := range ingress.Routes {
			lines = append(lines, ingress.Namespace+" · "+route.host+route.path)
			service, found := services[ingress.Namespace+"/"+route.service]
			serviceLabel := first(route.service, "unsupported backend")
			if route.port != "" {
				serviceLabel += ":" + route.port
			}
			var pods []string
			if found && len(service.Selector) > 0 {
				for _, pod := range a.snapshot.Resources["pods"] {
					if pod.Namespace != service.Namespace {
						continue
					}
					matches := true
					for name, value := range service.Selector {
						if actual, ok := pod.Labels[name]; !ok || actual != value {
							matches = false
							break
						}
					}
					if matches {
						status := ""
						if len(pod.Cells) > 2 {
							status = " [" + pod.Cells[2] + "]"
						}
						pods = append(pods, pod.Name+status)
					}
				}
			}
			if len(pods) == 0 {
				reason := "No matching pods"
				if !found {
					reason = "Service unavailable"
				} else if len(service.Selector) == 0 {
					reason = "External / selectorless service"
				}
				pods = []string{reason}
			}
			if cols < 66 {
				lines = append(lines, "┌ Ingress: "+ingress.Name, "└─→ Service: "+serviceLabel)
				for _, pod := range pods {
					lines = append(lines, "    └─→ Pod: "+pod)
				}
			} else {
				w := (cols - 8) / 3
				border := "┌" + strings.Repeat("─", w-2) + "┐"
				bottom := "└" + strings.Repeat("─", w-2) + "┘"
				box := func(label string) string { return "│" + fit(label, w-2) + "│" }
				lines = append(lines, border+"    "+border+"    "+border,
					box("Ingress")+"    "+box("Service")+"    "+box("Pod"),
					box(ingress.Name)+" ─→ "+box(serviceLabel)+" ─→ "+box(pods[0]),
					bottom+"    "+bottom+"    "+bottom)
				for _, pod := range pods[1:] {
					lines = append(lines, strings.Repeat(" ", w+4)+"└"+strings.Repeat("─", w+2)+"→ "+pod)
				}
				// Keep full names available when the boxes are narrow.
				lines = append(lines, fmt.Sprintf("%s → %s → %s", ingress.Name, serviceLabel, strings.Join(pods, ", ")))
			}
			lines = append(lines, "")
		}
	}
	if len(lines) == 0 {
		lines = []string{"No Ingress routes in this namespace."}
	}
	return lines
}
