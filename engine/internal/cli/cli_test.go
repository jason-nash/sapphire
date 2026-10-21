package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"sapphire/engine/internal/daemon"
	"sapphire/engine/internal/protocol"
)

func testHome(t *testing.T) string {
	t.Helper()
	path, err := os.MkdirTemp("/tmp", "sc-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(path) })
	t.Setenv("SAPPHIRE_HOME", path)
	t.Setenv("SAPPHIRE_MAC", "")
	return path
}

func TestCallErrorsAlwaysUseJSONEnvelope(t *testing.T) {
	testHome(t)
	for _, args := range [][]string{
		{"call", "not json"},
		{"call"},
		{"call", `{"action":"machine.list"}`},
		{"call", "--cpus", "0", `{"action":"machine.list"}`},
		{"call", `{"action":"status"} {}`},
	} {
		var stdout, stderr bytes.Buffer
		if code := Run(context.Background(), args, &stdout, &stderr); code == 0 {
			t.Fatalf("unexpected success: %v", args)
		}
		var response protocol.Response
		if err := protocol.Decode(&stdout, &response); err != nil || response.OK || response.Error == "" || string(response.Result) != "null" {
			t.Fatalf("%v: invalid envelope %+v %v; stderr %s", args, response, err, &stderr)
		}
	}
}

func TestStoppedStatusIsBareJSONAndReadOnly(t *testing.T) {
	home := testHome(t)
	var stdout, stderr bytes.Buffer
	code := Run(context.Background(), []string{"--home", home, "status", "--json"}, &stdout, &stderr)
	var value map[string]json.RawMessage
	err := json.Unmarshal(stdout.Bytes(), &value)
	if code != 0 || err != nil || value["ok"] != nil || string(value["backend"]) != `"hypervisor"` ||
		string(value["running"]) != "false" || string(value["pid"]) != "0" {
		t.Fatalf("status: %s code %d err %v stderr %s", &stdout, code, err, &stderr)
	}
	entries, _ := os.ReadDir(home)
	if len(entries) != 0 {
		t.Fatal("stopped status created runtime files")
	}
}

func TestStartChecksOnlyExplicitResources(t *testing.T) {
	home := testHome(t)
	listener, err := net.Listen("unix", filepath.Join(home, "control.sock"))
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(daemon.HostStatus{
			Running: true, PID: 123, CPUs: 2, MemoryMiB: 1024, State: "running", Backend: "hypervisor",
		})
	})}
	go server.Serve(listener)
	defer server.Close()
	for _, test := range []struct {
		args []string
		ok   bool
	}{
		{[]string{"start", "--json"}, true},
		{[]string{"start", "--cpus", "2", "--memory-mib", "1024", "--json"}, true},
		{[]string{"--cpus", "4", "start", "--json"}, false},
		{[]string{"start", "--memory-mib", "4096", "--json"}, false},
	} {
		var stdout, stderr bytes.Buffer
		code := Run(context.Background(), test.args, &stdout, &stderr)
		if (code == 0) != test.ok {
			t.Fatalf("%v: code %d stderr %s", test.args, code, &stderr)
		}
		if test.ok {
			var status daemon.HostStatus
			json.Unmarshal(stdout.Bytes(), &status)
			if status.CPUs != 2 || status.MemoryMiB != 1024 {
				t.Fatalf("reported defaults instead of actual VM resources: %+v", status)
			}
		} else if !strings.Contains(stderr.String(), "2 CPUs and 1024 MiB") {
			t.Fatalf("conflict did not describe actual limits: %s", &stderr)
		}
	}
}
