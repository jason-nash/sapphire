package guest

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
)

type ServiceStatus struct {
	Running   bool   `json:"running"`
	Ready     bool   `json:"ready"`
	PID       int    `json:"pid,omitempty"`
	Available bool   `json:"available"`
	Version   string `json:"version,omitempty"`
	Log       string `json:"log"`
	Error     string `json:"error,omitempty"`
}

type services struct {
	mu         sync.Mutex
	supervisor *supervisor
	docker     *child
	k3s        *child
}

func childRunning(c *child) bool {
	if c == nil {
		return false
	}
	select {
	case <-c.done:
		return false
	default:
		return true
	}
}

func dockerReady(ctx context.Context) bool {
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", "/var/run/docker.sock")
		},
	}
	defer transport.CloseIdleConnections()
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://docker/_ping", nil)
	if err != nil {
		return false
	}
	resp, err := (&http.Client{Transport: transport}).Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 16))
	return resp.StatusCode == http.StatusOK && strings.TrimSpace(string(data)) == "OK"
}

func (s *services) dockerStatus(ctx context.Context) ServiceStatus {
	s.mu.Lock()
	c := s.docker
	s.mu.Unlock()
	status := ServiceStatus{Available: true, Log: "/var/log/sapphire/docker.log"}
	if childRunning(c) {
		status.Running, status.PID = true, c.identity.PID
		status.Ready = dockerReady(ctx)
	}
	return status
}

func (s *services) spawn(path string, args []string, logName string) (*child, error) {
	logFile, err := os.OpenFile("/var/log/sapphire/"+logName+".log", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	defer logFile.Close()
	null, err := os.Open(os.DevNull)
	if err != nil {
		return nil, err
	}
	defer null.Close()
	attrs, cg, err := serviceAttributes(logName)
	if err != nil {
		return nil, err
	}
	defer cg.Close()
	c, err := s.supervisor.start(path, args, guestEnvironment(), []*os.File{null, logFile, logFile}, attrs)
	if err == nil {
		logChild(logName, c)
	}
	return c, err
}

func (s *services) startDocker(ctx context.Context) (ServiceStatus, error) {
	if err := ValidateWallClock(time.Now()); err != nil {
		return ServiceStatus{}, err
	}
	if err := ctx.Err(); err != nil {
		return ServiceStatus{}, err
	}
	s.mu.Lock()
	if !childRunning(s.docker) {
		if dockerReady(ctx) {
			s.mu.Unlock()
			return ServiceStatus{}, errors.New("a Docker daemon is already listening but is not owned by the guest supervisor")
		}
		// Only our guest-owned Unix endpoint is exposed; no TCP daemon listener.
		c, err := s.spawn("dockerd", []string{
			"--host=unix:///var/run/docker.sock", "--data-root=/var/lib/docker",
			"--exec-root=/run/docker", "--pidfile=/run/sapphire/dockerd.pid",
			"--exec-opt=native.cgroupdriver=cgroupfs", "--cgroup-parent=/sapphire/docker",
			"--default-cgroupns-mode=private",
		}, "docker")
		if err != nil {
			s.mu.Unlock()
			return ServiceStatus{}, err
		}
		s.docker = c
	}
	c := s.docker
	s.mu.Unlock()
	if err := waitReady(ctx, c, dockerReady); err != nil {
		return s.dockerStatus(ctx), fmt.Errorf("Docker not ready (see docker.log): %w", err)
	}
	return s.dockerStatus(ctx), nil
}

func (s *services) stopDocker(ctx context.Context) (ServiceStatus, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if childRunning(s.k3s) {
		return ServiceStatus{}, errors.New("stop Kubernetes before stopping its Docker runtime")
	}
	var stopErr error
	if childRunning(s.docker) {
		grace, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		stopErr = s.supervisor.stop(grace, s.docker)
	}
	// dockerd/containerd helpers and --cgroup-parent workloads occupy separate
	// subtrees. Both must drain after even a forced daemon exit.
	cleanup, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	cleanupErr := errors.Join(
		killAndDrainCgroup(cleanup, CgroupPath+"/services/docker"),
		killAndDrainCgroup(cleanup, CgroupPath+"/docker"),
	)
	if err := errors.Join(stopErr, cleanupErr); err != nil {
		return ServiceStatus{}, err
	}
	return ServiceStatus{Available: true, Log: "/var/log/sapphire/docker.log"}, nil
}

func killAndDrainCgroup(ctx context.Context, path string) error {
	if !populated(path) {
		return nil
	}
	if err := writeValue(path+"/cgroup.kill", "1"); err != nil {
		return err
	}
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for populated(path) {
		select {
		case <-ctx.Done():
			return fmt.Errorf("%s still has processes after cgroup.kill: %w", path, ctx.Err())
		case <-ticker.C:
		}
	}
	return nil
}

func waitReady(ctx context.Context, c *child, check func(context.Context) bool) error {
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		if check(ctx) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-c.done:
			return fmt.Errorf("service exited with status %d", c.exitCode)
		case <-ticker.C:
		}
	}
}

type k3sManifest struct {
	Version string `json:"version"`
	SHA256  string `json:"sha256"`
}

const k3sNodeName = "sapphire"

func installedK3s() (k3sManifest, error) {
	var manifest k3sManifest
	data, err := os.ReadFile("/usr/share/sapphire/k3s.json")
	if err != nil {
		return manifest, errors.New("optional k3s asset is not installed; build the guest with INCLUDE_K3S=1 and an exact version/SHA256")
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		return manifest, err
	}
	if !strings.HasPrefix(manifest.Version, "v") || len(manifest.SHA256) != 64 {
		return manifest, errors.New("invalid pinned k3s manifest")
	}
	return manifest, nil
}

func verifyK3s(manifest k3sManifest) error {
	file, err := os.Open("/usr/local/bin/k3s")
	if err != nil {
		return err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return err
	}
	if hex.EncodeToString(hash.Sum(nil)) != manifest.SHA256 {
		return errors.New("k3s asset checksum mismatch")
	}
	return nil
}

func k3sReady(ctx context.Context) bool {
	ca, err := os.ReadFile("/var/lib/rancher/k3s/server/tls/server-ca.crt")
	if err != nil {
		return false
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		return false
	}
	cert, err := tls.LoadX509KeyPair("/var/lib/rancher/k3s/server/tls/client-admin.crt", "/var/lib/rancher/k3s/server/tls/client-admin.key")
	if err != nil {
		return false
	}
	transport := &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: pool, Certificates: []tls.Certificate{cert}}}
	defer transport.CloseIdleConnections()
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	return k3sWorkloadReady(ctx, &http.Client{Transport: transport}, "https://127.0.0.1:6443")
}

func k3sWorkloadReady(ctx context.Context, client *http.Client, endpoint string) bool {
	get := func(path string, result any) bool {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+path, nil)
		if err != nil {
			return false
		}
		resp, err := client.Do(req)
		if err != nil {
			return false
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return false
		}
		if result == nil {
			_, err := io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
			return err == nil
		}
		return json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(result) == nil
	}
	if !get("/readyz", nil) {
		return false
	}
	// The API can be healthy before kubelet registration and the service-account
	// controller finish bootstrapping. Require both before reporting ready.
	var node struct {
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
		Status struct {
			Conditions []struct {
				Type   string `json:"type"`
				Status string `json:"status"`
			} `json:"conditions"`
		} `json:"status"`
	}
	if !get("/api/v1/nodes/"+k3sNodeName, &node) || node.Metadata.Name != k3sNodeName {
		return false
	}
	nodeReady := false
	for _, condition := range node.Status.Conditions {
		if condition.Type == "Ready" {
			nodeReady = condition.Status == "True"
			break
		}
	}
	if !nodeReady {
		return false
	}
	var account struct {
		Metadata struct {
			Name      string `json:"name"`
			Namespace string `json:"namespace"`
		} `json:"metadata"`
	}
	return get("/api/v1/namespaces/default/serviceaccounts/default", &account) &&
		account.Metadata.Name == "default" && account.Metadata.Namespace == "default"
}

func (s *services) k3sStatus(ctx context.Context) ServiceStatus {
	s.mu.Lock()
	c := s.k3s
	s.mu.Unlock()
	manifest, err := installedK3s()
	status := ServiceStatus{Available: err == nil, Version: manifest.Version, Log: "/var/log/sapphire/k3s.log"}
	if err != nil {
		status.Error = err.Error()
	}
	if childRunning(c) {
		status.Running, status.PID = true, c.identity.PID
		status.Ready = k3sReady(ctx)
	}
	return status
}

func (s *services) startK3s(ctx context.Context) (ServiceStatus, error) {
	manifest, err := installedK3s()
	if err != nil {
		return s.k3sStatus(ctx), err
	}
	if err := verifyK3s(manifest); err != nil {
		return s.k3sStatus(ctx), err
	}
	if _, err := s.startDocker(ctx); err != nil {
		return s.k3sStatus(ctx), err
	}
	s.mu.Lock()
	if !childRunning(s.k3s) {
		if err := writeValue("/var/lib/sapphire/k3s-active", manifest.Version+"\n"); err != nil {
			s.mu.Unlock()
			return ServiceStatus{}, err
		}
		// No --cluster-init/--datastore-endpoint: a single server uses SQLite.
		// --docker uses the one guest dockerd through embedded cri-dockerd.
		c, err := s.spawn("/usr/local/bin/k3s", []string{
			"server", "--docker", "--node-name=" + k3sNodeName,
			"--data-dir=/var/lib/rancher/k3s", "--write-kubeconfig-mode=0600",
			"--disable=traefik", "--disable=servicelb",
		}, "k3s")
		if err != nil {
			s.mu.Unlock()
			return ServiceStatus{}, err
		}
		s.k3s = c
	}
	c := s.k3s
	s.mu.Unlock()
	if err := waitReady(ctx, c, k3sReady); err != nil {
		return s.k3sStatus(ctx), fmt.Errorf("Kubernetes not ready (see k3s.log): %w", err)
	}
	return s.k3sStatus(ctx), nil
}

func (s *services) stopK3s(ctx context.Context) (ServiceStatus, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, markerErr := os.Stat("/var/lib/sapphire/k3s-active")
	hadCluster := s.k3s != nil || markerErr == nil
	if childRunning(s.k3s) {
		grace, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		if err := s.supervisor.stop(grace, s.k3s); err != nil {
			return ServiceStatus{}, err
		}
	}
	if populated(CgroupPath + "/services/k3s") {
		if err := writeValue(CgroupPath+"/services/k3s/cgroup.kill", "1"); err != nil {
			return ServiceStatus{}, err
		}
	}
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for populated(CgroupPath + "/services/k3s") {
		select {
		case <-ctx.Done():
			return ServiceStatus{}, ctx.Err()
		case <-deadline.C:
			return ServiceStatus{}, errors.New("Kubernetes helper cgroup did not drain")
		case <-ticker.C:
		}
	}
	if hadCluster {
		if !dockerReady(ctx) {
			return ServiceStatus{}, errors.New("Kubernetes helpers stopped, but Docker is unavailable to verify pod/sandbox cleanup")
		}
		if err := s.removeKubernetesContainers(ctx); err != nil {
			return ServiceStatus{}, err
		}
		if err := os.Remove("/var/lib/sapphire/k3s-active"); err != nil && !errors.Is(err, os.ErrNotExist) {
			return ServiceStatus{}, err
		}
	}
	manifest, err := installedK3s()
	return ServiceStatus{Available: err == nil, Version: manifest.Version, Log: "/var/log/sapphire/k3s.log"}, nil
}

type dockerInspection struct {
	ID     string `json:"Id"`
	Name   string `json:"Name"`
	Config struct {
		Labels map[string]string `json:"Labels"`
	} `json:"Config"`
}

var podUIDPattern = regexp.MustCompile(`^([0-9a-f]{32}|[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})$`)

func isKubernetesContainer(container dockerInspection) bool {
	labels := container.Config.Labels
	uid := labels["io.kubernetes.pod.uid"]
	name := strings.TrimPrefix(container.Name, "/")
	return podUIDPattern.MatchString(uid) &&
		labels["io.kubernetes.pod.name"] != "" &&
		labels["io.kubernetes.pod.namespace"] != "" &&
		labels["io.kubernetes.container.name"] != "" &&
		strings.HasPrefix(name, "k8s_") && strings.Contains(name, "_"+uid+"_")
}

func (s *services) kubernetesContainers(ctx context.Context) ([]string, error) {
	list, err := s.supervisor.capture(ctx, "docker", []string{"--host", "unix:///var/run/docker.sock",
		"ps", "-a", "--no-trunc", "--filter", "label=io.kubernetes.pod.uid", "--format", "{{.ID}}"}, nil, nil)
	if err != nil || list.ExitCode != 0 || list.Truncated {
		return nil, fmt.Errorf("list Kubernetes Docker containers: %v %s", err, list.Stderr)
	}
	var owned []string
	for _, id := range strings.Fields(list.Stdout) {
		if len(id) != 64 {
			return nil, errors.New("Docker returned an invalid container ID")
		}
		result, err := s.supervisor.capture(ctx, "docker", []string{"--host", "unix:///var/run/docker.sock", "inspect", "--type", "container", id}, nil, nil)
		if err != nil || result.ExitCode != 0 || result.Truncated {
			return nil, fmt.Errorf("inspect Kubernetes Docker container: %v %s", err, result.Stderr)
		}
		var inspections []dockerInspection
		if err := json.Unmarshal([]byte(result.Stdout), &inspections); err != nil {
			return nil, err
		}
		if len(inspections) != 1 || inspections[0].ID != id {
			return nil, errors.New("Docker inspection identity mismatch")
		}
		if !isKubernetesContainer(inspections[0]) {
			return nil, fmt.Errorf("container %s has partial or inconsistent Kubernetes labels; refusing to remove it or report a clean cluster stop", id)
		}
		owned = append(owned, id)
	}
	return owned, nil
}

func (s *services) removeKubernetesContainers(ctx context.Context) error {
	owned, err := s.kubernetesContainers(ctx)
	if err != nil {
		return err
	}
	for _, id := range owned {
		// No --volumes: persistent workload volumes survive a cluster stop.
		if err := s.supervisor.command(ctx, "docker", "--host", "unix:///var/run/docker.sock", "rm", "--force", id); err != nil {
			return fmt.Errorf("remove Kubernetes container/sandbox %s: %w", id, err)
		}
	}
	remaining, err := s.kubernetesContainers(ctx)
	if err != nil {
		return err
	}
	if len(remaining) != 0 {
		return errors.New("Kubernetes containers or sandboxes remain after shutdown")
	}
	return nil
}

func (s *services) kubeconfig(ctx context.Context) (string, error) {
	status := s.k3sStatus(ctx)
	if !status.Ready {
		return "", errors.New("Kubernetes is not ready")
	}
	file, err := os.Open("/etc/rancher/k3s/k3s.yaml")
	if err != nil {
		return "", err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, 1<<20))
	return string(data), err
}
