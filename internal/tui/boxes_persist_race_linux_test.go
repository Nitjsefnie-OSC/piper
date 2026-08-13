//go:build linux

package tui

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/piperbox/piper/internal/config"
	"github.com/piperbox/piper/internal/relayclient"
)

func TestLegacyIdentityMigrationCannotOverwriteConcurrentConfigWriter(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	oldBox := config.Box{
		Name: "old", Addr: "192.168.1.6:8088",
		RelayAPI: "https://old-relay.example", AccountCredential: "old-cred",
	}
	oldFile := config.ClientFile{Boxes: []config.Box{oldBox}, Current: "old"}
	if err := config.SaveClientFile(oldFile); err != nil {
		t.Fatal(err)
	}

	configPath := filepath.Join(os.Getenv("HOME"), ".piper", "piper", "config.json")
	fd, err := syscall.InotifyInit()
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.Close(fd)
	if _, err := syscall.InotifyAddWatch(fd, configPath, syscall.IN_OPEN); err != nil {
		t.Fatal(err)
	}

	replaced := make(chan error, 1)
	go func() {
		buf := make([]byte, syscall.SizeofInotifyEvent+256)
		if _, err := syscall.Read(fd, buf); err != nil {
			replaced <- err
			return
		}
		replaced <- config.SaveClientFile(config.ClientFile{
			Boxes: []config.Box{{
				Name: "old", Addr: "192.168.1.6:8088",
				RelayAPI: "https://new-relay.example", AccountCredential: "new-cred",
			}},
			Current: "old",
		})
	}()

	const base = "old-agent.public.example"
	agents := make([]relayclient.Agent, 200_000)
	for i := range agents {
		agents[i].BaseDomain = "noise.invalid"
	}
	agents[len(agents)-1].BaseDomain = base
	identities := map[string]string{boxIdentityKey(oldBox): base}
	_, accepted, err := persistLegacyIdentities(
		relayCredentialHash(oldFile), oldBox.RelayAPI, oldBox.AccountCredential,
		agents, identities,
	)
	if err != nil {
		t.Fatalf("migration error = %v", err)
	}
	if accepted {
		t.Fatalf("stale relay identity migration was accepted after a concurrent config replacement")
	}
	if err := <-replaced; err != nil {
		t.Fatal(err)
	}
	got, err := config.LoadClientFile()
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Boxes) != 1 || got.Boxes[0].RelayAPI != "https://new-relay.example" || got.Boxes[0].AccountCredential != "new-cred" {
		t.Fatalf("concurrent credential replacement was overwritten: %+v", got)
	}
	if got.Boxes[0].BaseDomain != "" {
		t.Fatalf("stale identity was persisted after concurrent replacement: %+v", got)
	}
}
