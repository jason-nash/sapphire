package daemon

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"slices"
	"testing"

	"sapphire/engine/internal/config"
)

func assertInitializationProvenance(t *testing.T, client *http.Client, want bool) {
	t.Helper()
	response := callHTTP(t, client, `{"action":"guest.fixture.launch"}`)
	var launch struct {
		InitializeDataDisk *bool `json:"initializeDataDisk"`
	}
	err := json.Unmarshal(response.Result, &launch)
	if !response.OK || err != nil || launch.InitializeDataDisk == nil || *launch.InitializeDataDisk != want {
		t.Fatalf("VMM initialization provenance: want %v, response %+v, error %v", want, response, err)
	}
}

func TestPrepareFreshDataDiskPermitsInitializationOnlyOnce(t *testing.T) {
	c := fixtureConfig(t)
	created, err := prepare(c)
	if err != nil || !created {
		t.Fatalf("fresh data disk: created %v, error %v", created, err)
	}
	args := driverArguments(c, created)
	count := 0
	for _, arg := range args {
		if arg == "--initialize-data-disk" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("fresh disk must get exactly one initialization switch: %v", args)
	}
	created, err = prepare(c)
	if err != nil || created || slices.Contains(driverArguments(c, created), "--initialize-data-disk") {
		t.Fatalf("reused zero-filled disk gained initialization permission: created %v, error %v", created, err)
	}
}

func TestPrepareExistingDataNeverPermitsInitialization(t *testing.T) {
	for _, nonzero := range []bool{false, true} {
		name := "zero"
		if nonzero {
			name = "nonzero"
		}
		t.Run(name, func(t *testing.T) {
			c := fixtureConfig(t)
			file, err := os.OpenFile(c.Path("data.img"), os.O_CREATE|os.O_RDWR|os.O_EXCL, 0600)
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			if err := file.Truncate(c.DataGiB * config.GiB); err != nil {
				t.Fatal(err)
			}
			marker := []byte("existing data must survive")
			if nonzero {
				if _, err := file.WriteAt(marker, 4096); err != nil {
					t.Fatal(err)
				}
			}
			created, err := prepare(c)
			if err != nil || created || slices.Contains(driverArguments(c, created), "--initialize-data-disk") {
				t.Fatalf("existing disk gained initialization permission: created %v, error %v", created, err)
			}
			if nonzero {
				persisted := make([]byte, len(marker))
				if _, err := file.ReadAt(persisted, 4096); err != nil || !bytes.Equal(persisted, marker) {
					t.Fatalf("existing disk changed: %q %v", persisted, err)
				}
			}
		})
	}
}

func TestFailedPrepareDoesNotPreserveInitializationPermission(t *testing.T) {
	c := fixtureConfig(t)
	if err := os.Mkdir(c.Path("console.log"), 0700); err != nil {
		t.Fatal(err)
	}
	created, err := prepare(c)
	if err == nil || created {
		t.Fatalf("failed preparation granted initialization: created %v, error %v", created, err)
	}
	if info, err := os.Stat(c.Path("data.img")); err != nil || info.Size() != c.DataGiB*config.GiB {
		t.Fatalf("test did not fail after disk creation: %v %v", info, err)
	}
	if err := os.Remove(c.Path("console.log")); err != nil {
		t.Fatal(err)
	}
	created, err = prepare(c)
	if err != nil || created || slices.Contains(driverArguments(c, created), "--initialize-data-disk") {
		t.Fatalf("retry reused stale creation provenance: created %v, error %v", created, err)
	}
}
