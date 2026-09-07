package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

type resourceType struct {
	key, kind, title string
	columns          []string
}

var resourceTypes = []resourceType{
	{"pods", "Pod", "Pods", []string{"NAME", "READY", "STATUS", "RESTARTS", "NODE", "IP"}},
	{"deployments", "Deployment", "Deployments", []string{"NAME", "READY", "STATUS", "UPDATED", "AVAILABLE", "REVISION"}},
	{"statefulsets", "StatefulSet", "StatefulSets", []string{"NAME", "READY", "STATUS", "UPDATED", "REVISION"}},
	{"daemonsets", "DaemonSet", "DaemonSets", []string{"NAME", "READY", "STATUS", "UPDATED", "AVAILABLE"}},
	{"jobs", "Job", "Jobs", []string{"NAME", "COMPLETE", "STATUS", "ACTIVE", "FAILED"}},
	{"cronjobs", "CronJob", "CronJobs", []string{"NAME", "SCHEDULE", "STATUS", "ACTIVE", "LAST RUN"}},
	{"services", "Service", "Services", []string{"NAME", "TYPE", "CLUSTER-IP", "EXTERNAL-IP", "PORTS"}},
	{"ingresses", "Ingress", "Ingresses", []string{"NAME", "CLASS", "HOSTS", "ADDRESS"}},
	{"persistentvolumeclaims", "PersistentVolumeClaim", "Volumes", []string{"NAME", "STATUS", "CAPACITY", "CLASS", "VOLUME"}},
	{"events", "Event", "Events", []string{"OBJECT", "TYPE", "REASON", "COUNT", "MESSAGE"}},
	{"secrets", "Secret", "Secrets", []string{"NAME", "TYPE", "DATA"}},
	{"configmaps", "ConfigMap", "ConfigMaps", []string{"NAME", "DATA"}},
	{"nodes", "Node", "Nodes", []string{"NAME", "STATUS", "VERSION", "ADDRESS", "CPU CAP", "MEM CAP"}},
}

type metadata struct {
	Name              string            `json:"name"`
	Namespace         string            `json:"namespace"`
	ResourceVersion   string            `json:"resourceVersion"`
	UID               string            `json:"uid"`
	Generation        int64             `json:"generation"`
	CreationTimestamp time.Time         `json:"creationTimestamp"`
	DeletionTimestamp *time.Time        `json:"deletionTimestamp"`
	Annotations       map[string]string `json:"annotations"`
	Labels            map[string]string `json:"labels"`
}
type record struct {
	metadata
	Cells                    []string
	Search, Tone, Node, Host string
	Containers               []string
	Seen                     time.Time
	Limits                   map[string]float64
	Capacity                 map[string]string
	Selector                 map[string]string
	Routes                   []ingressRoute
}

type ingressBackend struct {
	Service struct {
		Name string
		Port struct {
			Name   string
			Number int
		}
	}
}
type ingressRoute struct{ host, path, service, port string }

func (r record) id() string { return first(r.UID, r.Namespace+"/"+r.Name) }

type snapshot struct {
	Namespaces []string
	Resources  map[string][]record
	Errors     map[string]string
	Updated    time.Time
}
type objectList struct {
	Kind  string            `json:"kind"`
	Items []json.RawMessage `json:"items"`
}
type resourceRequirements struct{ Limits map[string]string }
type containerSpec struct {
	Name      string
	Resources resourceRequirements
}
type object struct {
	Kind             string
	Metadata         metadata
	Data, BinaryData map[string]string
	Spec             struct {
		Resources                                                                           resourceRequirements
		NodeName, Type, ClusterIP, Schedule, IngressClassName, StorageClassName, VolumeName string
		Replicas, Completions                                                               *int
		Suspend, Unschedulable                                                              bool
		Containers, InitContainers, EphemeralContainers                                     []containerSpec
		ExternalIPs                                                                         []string
		Selector                                                                            json.RawMessage
		DefaultBackend                                                                      ingressBackend
		Ports                                                                               []struct {
			Protocol       string
			Port, NodePort int
		}
		Rules []struct {
			Host string
			HTTP struct {
				Paths []struct {
					Path    string
					Backend ingressBackend
				}
			}
		}
	}
	Status struct {
		Phase, PodIP, CurrentRevision, UpdateRevision                                string
		Replicas, ReadyReplicas, UpdatedReplicas, AvailableReplicas                  int
		DesiredNumberScheduled, NumberReady, UpdatedNumberScheduled, NumberAvailable int
		Succeeded, Failed                                                            int
		Active                                                                       json.RawMessage
		ObservedGeneration                                                           int64
		LastScheduleTime                                                             time.Time
		Capacity                                                                     map[string]string
		Conditions                                                                   []struct{ Type, Status, Reason string }
		Addresses                                                                    []struct{ Type, Address string }
		NodeInfo                                                                     struct{ KubeletVersion string }
		ContainerStatuses                                                            []struct {
			Ready        bool
			RestartCount int
			State        map[string]json.RawMessage
		}
		InitContainerStatuses []struct {
			Ready        bool
			RestartCount int
			State        map[string]json.RawMessage
		}
		LoadBalancer struct {
			Ingress []struct{ IP, Hostname string }
		}
	}
	Type, Reason, Message    string
	Count                    int
	LastTimestamp, EventTime time.Time
	Series                   struct {
		Count            int
		LastObservedTime time.Time
	}
	InvolvedObject struct{ Kind, Name string }
}

func kubectl(ctx context.Context, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "kubectl", args...)
	cmd.WaitDelay = 2 * time.Second
	if os.Getenv("KUBECONFIG") == "" {
		if home, err := os.UserHomeDir(); err == nil {
			path := filepath.Join(home, ".kube", "config")
			if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() {
				cmd.Env = append(os.Environ(), "KUBECONFIG="+path)
			}
		}
	}
	return cmd
}
func clusterCommand(ctx context.Context, name string, args ...string) *exec.Cmd {
	if name != "" {
		args = append([]string{"--context", name}, args...)
	}
	return kubectl(ctx, args...)
}
func currentContext(ctx context.Context) string {
	out, err := kubectl(ctx, "config", "current-context").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
func loadCluster(ctx context.Context, api *kubeAPI) snapshot {
	result := snapshot{Resources: map[string][]record{}, Errors: map[string]string{}, Updated: time.Now()}
	var mu sync.Mutex
	var wg sync.WaitGroup
	slots := make(chan struct{}, 4)
	keys := []string{"namespaces"}
	for _, kind := range resourceTypes {
		keys = append(keys, kind.key)
	}
	// Bound concurrent list requests; authentication and connections are shared through one proxy.
	for _, key := range keys {
		wg.Go(func() {
			select {
			case slots <- struct{}{}:
			case <-ctx.Done():
				mu.Lock()
				result.Errors[key] = ctx.Err().Error()
				mu.Unlock()
				return
			}
			defer func() { <-slots }()
			request, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			list := objectList{Kind: "NamespaceList"}
			for _, resource := range resourceTypes {
				if resource.key == key {
					list.Kind = resource.kind + "List"
				}
			}
			_, err := api.list(request, key, func(raw json.RawMessage) error { list.Items = append(list.Items, raw); return nil })
			var decoded snapshot
			if err == nil {
				data, _ := json.Marshal(list)
				decoded, err = decodeSnapshot(data)
			}
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				result.Errors[key] = err.Error()
				return
			}
			if key == "namespaces" {
				result.Namespaces = decoded.Namespaces
			} else {
				result.Resources[key] = decoded.Resources[key]
			}
		})
	}
	wg.Wait()
	return result
}
func decodeSnapshot(data []byte) (snapshot, error) {
	var list objectList
	if err := json.Unmarshal(data, &list); err != nil {
		return snapshot{}, fmt.Errorf("decode kubectl output: %w", err)
	}
	result := snapshot{Resources: map[string][]record{}}
	namespaces := map[string]bool{}
	for _, raw := range list.Items {
		var item object
		if err := json.Unmarshal(raw, &item); err != nil {
			return snapshot{}, fmt.Errorf("decode Kubernetes object: %w", err)
		}
		if item.Kind == "" {
			item.Kind = strings.TrimSuffix(list.Kind, "List")
		}
		if item.Kind == "Namespace" {
			namespaces[item.Metadata.Name] = true
			continue
		}
		for _, kind := range resourceTypes {
			if kind.kind != item.Kind {
				continue
			}
			r := item.record()
			result.Resources[kind.key] = append(result.Resources[kind.key], r)
			if r.Namespace != "" {
				namespaces[r.Namespace] = true
			}
			break
		}
	}
	for namespace := range namespaces {
		result.Namespaces = append(result.Namespaces, namespace)
	}
	sort.Strings(result.Namespaces)
	for key, rows := range result.Resources {
		sort.Slice(rows, func(i, j int) bool {
			if key == "events" && !rows[i].Seen.Equal(rows[j].Seen) {
				return rows[i].Seen.After(rows[j].Seen)
			}
			return rows[i].Namespace+"\x00"+rows[i].Name < rows[j].Namespace+"\x00"+rows[j].Name
		})
	}
	return result, nil
}
func (o object) record() record {
	r := record{metadata: o.Metadata, Seen: o.Metadata.CreationTimestamp, Node: o.Spec.NodeName}
	s := o.Status
	r.Cells = []string{r.Name}
	add := func(values ...string) { r.Cells = append(r.Cells, values...) }
	ratio := func(a, b int) string { return fmt.Sprintf("%d/%d", a, b) }
	rollout := func(ready, desired, updated int) string {
		if s.ObservedGeneration < r.Generation || ready < desired || updated < desired {
			r.Tone = "warn"
			return "Progressing"
		}
		return "Ready"
	}
	addresses := append([]string{}, o.Spec.ExternalIPs...)
	for _, a := range s.LoadBalancer.Ingress {
		addresses = append(addresses, first(a.IP, a.Hostname))
	}
	switch o.Kind {
	case "Pod":
		r.Limits = map[string]float64{}
		for _, resource := range []string{"cpu", "memory"} {
			total := float64(0)
			for _, container := range o.Spec.Containers {
				limit := quantity(container.Resources.Limits[resource])
				if limit <= 0 {
					total = 0
					break
				}
				total += limit
			}
			if limit := quantity(o.Spec.Resources.Limits[resource]); limit > 0 {
				total = limit
			}
			r.Limits[resource] = total
		}
		for _, c := range o.Spec.Containers {
			r.Containers = append(r.Containers, c.Name)
		}
		for _, c := range o.Spec.InitContainers {
			r.Containers = append(r.Containers, c.Name)
		}
		for _, c := range o.Spec.EphemeralContainers {
			r.Containers = append(r.Containers, c.Name)
		}
		ready, restarts, total := 0, 0, len(o.Spec.Containers)
		if total == 0 {
			total = len(s.ContainerStatuses)
		}
		status := s.Phase
		for _, c := range s.ContainerStatuses {
			if c.Ready {
				ready++
			}
			restarts += c.RestartCount
			if !c.Ready {
				status = first(stateReason(c.State), status)
			}
		}
		for _, c := range s.InitContainerStatuses {
			restarts += c.RestartCount
			if reason := stateReason(c.State); reason != "" && reason != "Completed" {
				status = "Init:" + reason
			}
		}
		if r.DeletionTimestamp != nil {
			status = "Terminating"
		}
		if (status != "Running" || ready < total) && status != "Succeeded" && status != "Completed" {
			r.Tone = "warn"
		}
		add(ratio(ready, total), status, fmt.Sprint(restarts), first(r.Node, "-"), first(s.PodIP, "-"))
	case "Deployment", "StatefulSet":
		desired := 1
		if o.Spec.Replicas != nil {
			desired = *o.Spec.Replicas
		}
		state := rollout(s.ReadyReplicas, desired, s.UpdatedReplicas)
		if o.Kind == "Deployment" && s.AvailableReplicas < desired {
			state, r.Tone = "Progressing", "warn"
		}
		for _, c := range s.Conditions {
			if (c.Type == "Progressing" && c.Status == "False") || (c.Type == "ReplicaFailure" && c.Status == "True") {
				state = c.Reason
				r.Tone = "bad"
			}
		}
		add(ratio(s.ReadyReplicas, desired), state, fmt.Sprint(s.UpdatedReplicas))
		if o.Kind == "Deployment" {
			add(fmt.Sprint(s.AvailableReplicas), first(r.Annotations["deployment.kubernetes.io/revision"], "-"))
		} else {
			add(first(s.UpdateRevision, "-"))
		}
	case "DaemonSet":
		add(ratio(s.NumberReady, s.DesiredNumberScheduled), rollout(min(s.NumberReady, s.NumberAvailable), s.DesiredNumberScheduled, s.UpdatedNumberScheduled), fmt.Sprint(s.UpdatedNumberScheduled), fmt.Sprint(s.NumberAvailable))
	case "Job":
		desired := 1
		if o.Spec.Completions != nil {
			desired = *o.Spec.Completions
		}
		state := "Running"
		for _, c := range s.Conditions {
			if c.Status == "True" && (c.Type == "Complete" || c.Type == "Failed") {
				state = c.Type
			}
		}
		if state == "Failed" {
			r.Tone = "bad"
		}
		var active int
		_ = json.Unmarshal(s.Active, &active)
		add(ratio(s.Succeeded, desired), state, fmt.Sprint(active), fmt.Sprint(s.Failed))
	case "CronJob":
		state := "Scheduled"
		if o.Spec.Suspend {
			state = "Suspended"
			r.Tone = "warn"
		}
		var active []json.RawMessage
		_ = json.Unmarshal(s.Active, &active)
		last := "-"
		if !s.LastScheduleTime.IsZero() {
			last = s.LastScheduleTime.Format(time.RFC3339)
		}
		add(o.Spec.Schedule, state, fmt.Sprint(len(active)), last)
	case "Service":
		// Workloads use a structured selector; only Services use a string map.
		if err := json.Unmarshal(o.Spec.Selector, &r.Selector); err != nil {
			r.Selector = nil
		}
		var ports []string
		for _, p := range o.Spec.Ports {
			v := fmt.Sprintf("%d/%s", p.Port, p.Protocol)
			if p.NodePort > 0 {
				v = fmt.Sprintf("%d:%d/%s", p.Port, p.NodePort, p.Protocol)
			}
			ports = append(ports, v)
		}
		add(o.Spec.Type, first(o.Spec.ClusterIP, "-"), first(strings.Join(addresses, ","), "-"), strings.Join(ports, ","))
	case "Ingress":
		var hosts []string
		addRoute := func(host, path string, backend ingressBackend) {
			port := backend.Service.Port.Name
			if backend.Service.Port.Number != 0 {
				port = fmt.Sprint(backend.Service.Port.Number)
			}
			r.Routes = append(r.Routes, ingressRoute{host: host, path: path, service: backend.Service.Name, port: port})
		}
		if o.Spec.DefaultBackend.Service.Name != "" {
			addRoute("*", "default", o.Spec.DefaultBackend)
		}
		for _, rule := range o.Spec.Rules {
			hosts = append(hosts, first(rule.Host, "*"))
			for _, path := range rule.HTTP.Paths {
				addRoute(first(rule.Host, "*"), first(path.Path, "/"), path.Backend)
			}
		}
		add(first(o.Spec.IngressClassName, "-"), strings.Join(hosts, ","), first(strings.Join(addresses, ","), "-"))
	case "PersistentVolumeClaim":
		if s.Phase != "Bound" {
			r.Tone = "warn"
		}
		add(s.Phase, first(s.Capacity["storage"], "-"), first(o.Spec.StorageClassName, "-"), first(o.Spec.VolumeName, "-"))
	case "Event":
		for _, stamp := range []time.Time{o.LastTimestamp, o.EventTime, o.Series.LastObservedTime} {
			if stamp.After(r.Seen) {
				r.Seen = stamp
			}
		}
		if o.Type == "Warning" {
			r.Tone = "warn"
		}
		r.Cells = []string{o.InvolvedObject.Kind + "/" + o.InvolvedObject.Name, o.Type, o.Reason, fmt.Sprint(max(o.Count, o.Series.Count, 1)), o.Message}
	case "Secret":
		add(o.Type, fmt.Sprint(len(o.Data)))
	case "ConfigMap":
		add(fmt.Sprint(len(o.Data) + len(o.BinaryData)))
	case "Node":
		r.Capacity = s.Capacity
		state := "NotReady"
		r.Tone = "bad"
		for _, c := range s.Conditions {
			if c.Type == "Ready" && c.Status == "True" {
				state = "Ready"
				r.Tone = ""
			}
		}
		for _, c := range s.Conditions {
			if c.Type != "Ready" && c.Status == "True" {
				state = c.Type
				r.Tone = "warn"
			}
		}
		if o.Spec.Unschedulable {
			state += "/Cordoned"
			r.Tone = "warn"
		}
		for _, kind := range []string{"ExternalIP", "InternalIP", "Hostname"} {
			for _, a := range s.Addresses {
				if a.Type == kind && r.Host == "" {
					r.Host = a.Address
				}
			}
		}
		add(state, s.NodeInfo.KubeletVersion, first(r.Host, "-"), s.Capacity["cpu"], s.Capacity["memory"])
	}
	r.Search = searchable(append([]string{r.Namespace, r.Name}, r.Cells...)...)
	return r
}
func stateReason(state map[string]json.RawMessage) string {
	for _, name := range []string{"waiting", "terminated"} {
		if raw := state[name]; len(raw) > 0 {
			var value struct{ Reason string }
			if json.Unmarshal(raw, &value) == nil && value.Reason != "" {
				return value.Reason
			}
		}
	}
	return ""
}
func searchable(parts ...string) string { return strings.ToLower(strings.Join(parts, "\x00")) }
func first(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func loadClusterSlots(ctx context.Context, current string) [5]string {
	var slots [5]string
	out, err := kubectl(ctx, "config", "get-contexts", "-o", "name").Output()
	if err != nil {
		slots[0] = current
		return slots
	}
	names := strings.Split(strings.TrimSpace(string(out)), "\n")
	found := false
	for i, name := range names {
		if i >= len(slots) {
			break
		}
		slots[i] = name
		if name == current {
			found = true
		}
	}
	if !found {
		for i, name := range slots {
			if name == "" {
				slots[i] = current
				return slots
			}
		}
		slots[4] = current
	}
	return slots
}
func clusterHelp(slots [5]string) string {
	text := "\nCLUSTERS · click the top-right slot or use Alt+1–5\n"
	for i, name := range slots {
		text += fmt.Sprintf("  %d  %s\n", i+1, first(name, "unassigned"))
	}
	return text + "  Set cluster_slots in config.json to pin the five contexts.\n"
}
