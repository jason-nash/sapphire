package dockerapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Container struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Image   string `json:"image"`
	State   string `json:"state"`
	Status  string `json:"status"`
	Ports   string `json:"ports"`
	Project string `json:"project"`
}

type Image struct {
	ID         string `json:"id"`
	Repository string `json:"repository"`
	Tag        string `json:"tag"`
	Size       string `json:"size"`
	Created    string `json:"created"`
}

type Volume struct {
	Name   string `json:"name"`
	Driver string `json:"driver"`
}

type Network struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Driver string `json:"driver"`
	Scope  string `json:"scope"`
}

type Project struct {
	Name        string `json:"Name"`
	Status      string `json:"Status"`
	ConfigFiles string `json:"ConfigFiles"`
}

type Inventory struct {
	Containers []Container `json:"containers"`
	Images     []Image     `json:"images"`
	Volumes    []Volume    `json:"volumes"`
	Networks   []Network   `json:"networks"`
	Projects   []Project   `json:"projects"`
}

type containerJSON struct {
	ID     string            `json:"Id"`
	Names  []string          `json:"Names"`
	Image  string            `json:"Image"`
	State  string            `json:"State"`
	Status string            `json:"Status"`
	Labels map[string]string `json:"Labels"`
	Ports  []struct {
		IP          string `json:"IP"`
		PrivatePort int    `json:"PrivatePort"`
		PublicPort  int    `json:"PublicPort"`
		Type        string `json:"Type"`
	} `json:"Ports"`
}

type imageJSON struct {
	ID      string   `json:"Id"`
	Tags    []string `json:"RepoTags"`
	Size    int64    `json:"Size"`
	Created int64    `json:"Created"`
}

// Read makes four concurrent, bounded API requests only when explicitly called.
// No Docker CLI process or Docker context is involved.
func Read(ctx context.Context, socket string) (Inventory, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socket)
		},
		MaxIdleConnsPerHost: 4, ResponseHeaderTimeout: 5 * time.Second,
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport}
	var containers []containerJSON
	var images []imageJSON
	var volumes struct {
		Volumes []Volume `json:"Volumes"`
	}
	var networks []Network
	endpoints := []struct {
		path   string
		output any
	}{
		{"/containers/json?all=1", &containers},
		{"/images/json", &images}, {"/volumes", &volumes}, {"/networks", &networks},
	}
	var wait sync.WaitGroup
	errors := make(chan error, len(endpoints))
	for _, endpoint := range endpoints {
		wait.Go(func() {
			request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://docker"+endpoint.path, nil)
			if err == nil {
				var response *http.Response
				response, err = client.Do(request)
				if err == nil {
					defer response.Body.Close()
					if response.StatusCode != http.StatusOK {
						err = fmt.Errorf("HTTP %d", response.StatusCode)
					} else {
						err = json.NewDecoder(io.LimitReader(response.Body, 16<<20)).Decode(endpoint.output)
					}
				}
			}
			if err != nil {
				errors <- fmt.Errorf("Docker %s: %w", endpoint.path, err)
			}
		})
	}
	wait.Wait()
	close(errors)
	for err := range errors {
		return Inventory{}, err
	}
	result := Inventory{
		Containers: []Container{}, Images: []Image{}, Volumes: volumes.Volumes,
		Networks: networks, Projects: []Project{},
	}
	if result.Volumes == nil {
		result.Volumes = []Volume{}
	}
	if result.Networks == nil {
		result.Networks = []Network{}
	}
	projects := make(map[string][]containerJSON)
	for _, container := range containers {
		name := container.ID
		if len(container.Names) > 0 {
			name = strings.TrimPrefix(container.Names[0], "/")
		}
		var ports []string
		for _, port := range container.Ports {
			label := strconv.Itoa(port.PrivatePort) + "/" + port.Type
			if port.PublicPort > 0 {
				ip := port.IP
				if ip == "" {
					ip = "0.0.0.0"
				}
				label = net.JoinHostPort(ip, strconv.Itoa(port.PublicPort)) + "->" + label
			}
			ports = append(ports, label)
		}
		sort.Strings(ports)
		project := container.Labels["com.docker.compose.project"]
		result.Containers = append(result.Containers, Container{
			ID: container.ID, Name: name, Image: container.Image,
			State: container.State, Status: container.Status,
			Ports: strings.Join(ports, ", "), Project: project,
		})
		if project != "" {
			projects[project] = append(projects[project], container)
		}
	}
	for _, image := range images {
		tags := image.Tags
		if len(tags) == 0 {
			tags = []string{"<none>:<none>"}
		}
		for _, reference := range tags {
			repository, tag := reference, "<none>"
			if colon := strings.LastIndex(reference, ":"); colon > strings.LastIndex(reference, "/") {
				repository, tag = reference[:colon], reference[colon+1:]
			}
			result.Images = append(result.Images, Image{
				ID: image.ID, Repository: repository, Tag: tag,
				Size:    humanBytes(image.Size),
				Created: time.Unix(image.Created, 0).UTC().Format(time.RFC3339),
			})
		}
	}
	for name, containers := range projects {
		running := 0
		configFiles := ""
		for _, container := range containers {
			if container.State == "running" {
				running++
			}
			if value := container.Labels["com.docker.compose.project.config_files"]; value != "" {
				configFiles = value
			}
		}
		status := fmt.Sprintf("stopped(%d)", len(containers))
		if running > 0 {
			status = fmt.Sprintf("running(%d)", running)
		}
		result.Projects = append(result.Projects, Project{Name: name, Status: status, ConfigFiles: configFiles})
	}
	sort.Slice(result.Containers, func(i, j int) bool { return result.Containers[i].Name < result.Containers[j].Name })
	sort.Slice(result.Projects, func(i, j int) bool { return result.Projects[i].Name < result.Projects[j].Name })
	return result, nil
}

func humanBytes(size int64) string {
	units := []string{"B", "kB", "MB", "GB", "TB"}
	value, unit := float64(size), 0
	for value >= 1000 && unit < len(units)-1 {
		value, unit = value/1000, unit+1
	}
	return strconv.FormatFloat(value, 'f', -1, 64) + units[unit]
}
