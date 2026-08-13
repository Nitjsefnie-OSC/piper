package config

import (
	"context"
	"os"
	"os/exec"
	"reflect"
	"testing"
	"time"
)

// The public UpdateClientFile contract permits a callback to compose another
// client-config writer. The nested writer must not wait on the callback's own
// lock; the outer update must report its stale snapshot without overwriting
// the nested commit.
func TestNestedClientConfigWritersDoNotDeadlock(t *testing.T) {
	if os.Getenv("PIPER_NESTED_CONFIG_WRITER_HELPER") == "1" {
		runNestedClientConfigWriters(t)
		return
	}

	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := SaveClientFile(ClientFile{
		Boxes:   []Box{{Name: "old", Addr: "old-addr", Token: "old-token"}},
		Current: "old",
	}); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestNestedClientConfigWritersDoNotDeadlock$")
	cmd.Env = append(os.Environ(),
		"HOME="+home,
		"PIPER_NESTED_CONFIG_WRITER_HELPER=1",
	)
	if err := cmd.Run(); ctx.Err() == context.DeadlineExceeded {
		t.Fatal("nested client-config writer deadlocked inside UpdateClientFile callback")
	} else if err != nil {
		t.Fatalf("nested client-config helper failed: %v", err)
	}
}

func runNestedClientConfigWriters(t *testing.T) {
	nestedFile := ClientFile{
		Boxes:   []Box{{Name: "nested", Addr: "nested-addr", Token: "nested-token"}},
		Current: "nested",
	}
	var nestedErr error
	outerErr := UpdateClientFile(func(cf *ClientFile) (bool, error) {
		nestedErr = SaveClientFile(nestedFile)
		return true, nil
	})
	if nestedErr != nil {
		t.Fatalf("nested SaveClientFile failed: %v", nestedErr)
	}
	if outerErr == nil {
		t.Fatal("outer UpdateClientFile silently committed over nested SaveClientFile")
	}
	got, err := LoadClientFile()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, nestedFile) {
		t.Fatalf("nested SaveClientFile result was overwritten: got %+v, want %+v", got, nestedFile)
	}

	base := ClientFile{
		Boxes:   []Box{{Name: "base", Addr: "base-addr", Token: "base-token"}},
		Current: "base",
	}
	if err := SaveClientFile(base); err != nil {
		t.Fatal(err)
	}
	var helperErr error
	outerErr = UpdateClientFile(func(cf *ClientFile) (bool, error) {
		helperErr = SaveClient(ClientConfig{Addr: "helper-addr", Token: "helper-token"})
		return true, nil
	})
	if helperErr != nil {
		t.Fatalf("nested SaveClient failed: %v", helperErr)
	}
	if outerErr == nil {
		t.Fatal("outer UpdateClientFile silently committed over nested SaveClient")
	}
	got, err = LoadClientFile()
	if err != nil {
		t.Fatal(err)
	}
	want := ClientFile{
		Boxes:   []Box{{Name: "base", Addr: "helper-addr", Token: "helper-token"}},
		Current: "base",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("nested SaveClient result was overwritten: got %+v, want %+v", got, want)
	}
}
