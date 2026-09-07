package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

type kubeAPI struct {
	client *http.Client
	base   string
	stop   context.CancelFunc
	cmd    *exec.Cmd
	dir    string
}

func resourcePath(kind string) string {
	group := "/api/v1/"
	switch kind {
	case "deployments", "statefulsets", "daemonsets":
		group = "/apis/apps/v1/"
	case "jobs", "cronjobs":
		group = "/apis/batch/v1/"
	case "ingresses":
		group = "/apis/networking.k8s.io/v1/"
	}
	return group + kind
}
func startAPI(ctx context.Context, cluster string) (*kubeAPI, error) {
	dir, err := os.MkdirTemp("", "kiwikube-api-")
	if err != nil {
		return nil, err
	}
	socket := filepath.Join(dir, "api.sock")
	processCtx, stop := context.WithCancel(ctx)
	cmd := clusterCommand(processCtx, cluster, "proxy", "--unix-socket="+socket, "--reject-methods=POST,PUT,PATCH,DELETE,CONNECT")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGTERM}
	// The private 0700 socket directory keeps the authenticated proxy off TCP and away from other users.
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err = cmd.Start(); err != nil {
		stop()
		os.RemoveAll(dir)
		return nil, err
	}
	transport := &http.Transport{MaxIdleConns: 128, MaxIdleConnsPerHost: 128, IdleConnTimeout: 90 * time.Second, ResponseHeaderTimeout: 15 * time.Second,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socket)
		},
	}
	api := &kubeAPI{client: &http.Client{Transport: transport}, base: "http://localhost", stop: stop, cmd: cmd, dir: dir}
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(25 * time.Millisecond)
	defer tick.Stop()
	for {
		if _, err = os.Stat(socket); err == nil {
			return api, nil
		}
		select {
		case <-ctx.Done():
			api.close()
			return nil, ctx.Err()
		case <-deadline.C:
			api.close()
			return nil, fmt.Errorf("Kubernetes proxy did not start: %s", stderr.String())
		case <-tick.C:
		}
	}
}
func (a *kubeAPI) close() {
	a.client.CloseIdleConnections()
	if a.stop != nil {
		a.stop()
		_ = a.cmd.Wait()
		_ = os.RemoveAll(a.dir)
	}
}

type apiError struct {
	code    int
	message string
}

func (e *apiError) Error() string { return fmt.Sprintf("Kubernetes HTTP %d: %s", e.code, e.message) }
func (a *kubeAPI) get(ctx context.Context, path string, query url.Values) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.base+path+"?"+query.Encode(), nil)
	if err != nil {
		return nil, err
	}
	response, err := a.client.Do(req)
	if err != nil {
		return nil, err
	}
	if response.StatusCode != http.StatusOK {
		defer response.Body.Close()
		body, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		return nil, &apiError{response.StatusCode, strings.TrimSpace(string(body))}
	}
	return response.Body, nil
}
func (a *kubeAPI) list(ctx context.Context, kind string, accept func(json.RawMessage) error) (string, error) {
	next := ""
	version := ""
	for {
		body, err := a.get(ctx, resourcePath(kind), url.Values{"limit": {"500"}, "continue": {next}})
		if err != nil {
			return "", err
		}
		var page struct {
			Metadata struct{ ResourceVersion, Continue string }
			Items    []json.RawMessage
		}
		err = json.NewDecoder(io.LimitReader(body, 64*1024*1024)).Decode(&page)
		body.Close()
		if err != nil {
			return "", err
		}
		for _, raw := range page.Items {
			if err = accept(raw); err != nil {
				return "", err
			}
		}
		version = page.Metadata.ResourceVersion
		next = page.Metadata.Continue
		if next == "" {
			return version, nil
		}
	}
}
