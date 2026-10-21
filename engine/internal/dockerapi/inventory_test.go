package dockerapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

func TestInventoryUsesDockerSocketAndConvertsUIModels(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "si-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	socket := filepath.Join(dir, "docker.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	requests := make(chan string, 4)
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests <- r.URL.RequestURI()
		switch r.URL.RequestURI() {
		case "/containers/json?all=1":
			fmt.Fprint(w, `[{"Id":"container","Names":["/web"],"Image":"nginx","State":"running","Status":"Up","Labels":{"com.docker.compose.project":"demo","com.docker.compose.project.config_files":"/tmp/compose.yml"},"Ports":[{"IP":"0.0.0.0","PrivatePort":80,"PublicPort":8080,"Type":"tcp"}]}]`)
		case "/images/json":
			fmt.Fprint(w, `[{"Id":"image","RepoTags":["localhost:5000/demo:latest"],"Size":12000000,"Created":1700000000}]`)
		case "/volumes":
			fmt.Fprint(w, `{"Volumes":[{"Name":"data","Driver":"local"}]}`)
		case "/networks":
			fmt.Fprint(w, `[{"Id":"network","Name":"bridge","Driver":"bridge","Scope":"local"}]`)
		default:
			http.NotFound(w, r)
		}
	})}
	go server.Serve(listener)
	defer server.Close()
	got, err := Read(context.Background(), socket)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Containers) != 1 || got.Containers[0].Name != "web" || got.Containers[0].Ports != "0.0.0.0:8080->80/tcp" ||
		len(got.Images) != 1 || got.Images[0].Repository != "localhost:5000/demo" || got.Images[0].Tag != "latest" ||
		len(got.Volumes) != 1 || got.Volumes[0].Name != "data" ||
		len(got.Networks) != 1 || got.Networks[0].ID != "network" ||
		len(got.Projects) != 1 || got.Projects[0].Name != "demo" {
		t.Fatalf("inventory conversion: %+v", got)
	}
	if len(requests) != 4 {
		t.Fatalf("unexpected API requests: %d", len(requests))
	}
	data, _ := json.Marshal(got)
	var raw map[string]json.RawMessage
	json.Unmarshal(data, &raw)
	for _, name := range []string{"containers", "images", "volumes", "networks", "projects"} {
		if len(raw[name]) == 0 {
			t.Fatalf("missing typed array %s", name)
		}
	}
}

func TestInventoryFailureDoesNotFakeEmptySuccess(t *testing.T) {
	if _, err := Read(context.Background(), "/tmp/sapphire-inventory-socket-does-not-exist"); err == nil {
		t.Fatal("unavailable Docker reported empty inventory")
	}
}
