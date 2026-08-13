package config

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
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
	if !errors.Is(outerErr, ErrClientConfigChanged) {
		t.Fatalf("outer UpdateClientFile error = %v, want ErrClientConfigChanged", outerErr)
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
	if !errors.Is(outerErr, ErrClientConfigChanged) {
		t.Fatalf("outer UpdateClientFile error = %v, want ErrClientConfigChanged", outerErr)
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

func TestNestedIdenticalAndABAWritesAreConflicts(t *testing.T) {
	base := ClientFile{
		Boxes:   []Box{{Name: "base", Addr: "base-addr", Token: "base-token"}},
		Current: "base",
	}
	nested := ClientFile{
		Boxes:   []Box{{Name: "nested", Addr: "nested-addr", Token: "nested-token"}},
		Current: "nested",
	}
	for _, tc := range []struct {
		name   string
		writes func() error
	}{
		{
			name: "identical",
			writes: func() error {
				return SaveClientFile(base)
			},
		},
		{
			name: "aba",
			writes: func() error {
				if err := SaveClientFile(nested); err != nil {
					return err
				}
				return SaveClientFile(base)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			if err := SaveClientFile(base); err != nil {
				t.Fatal(err)
			}

			var nestedErr error
			outerErr := UpdateClientFile(func(cf *ClientFile) (bool, error) {
				nestedErr = tc.writes()
				cf.Boxes[0].Token = "stale-outer-token"
				return true, nil
			})
			if nestedErr != nil {
				t.Fatalf("nested write failed: %v", nestedErr)
			}
			if !errors.Is(outerErr, ErrClientConfigChanged) {
				t.Fatalf("outer update error = %v, want ErrClientConfigChanged", outerErr)
			}
			got, err := LoadClientFile()
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, base) {
				t.Fatalf("nested write history was overwritten: got %+v, want %+v", got, base)
			}
		})
	}
}

func TestNestedWriterThroughHomeAliasDoesNotDeadlock(t *testing.T) {
	if os.Getenv("PIPER_HOME_ALIAS_WRITER_HELPER") == "1" {
		runNestedWriterThroughHomeAlias(t)
		return
	}

	realHome := t.TempDir()
	aliasRoot := t.TempDir()
	aliasA := filepath.Join(aliasRoot, "home-a")
	aliasB := filepath.Join(aliasRoot, "home-b")
	if err := os.Symlink(realHome, aliasA); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realHome, aliasB); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", aliasA)
	if err := SaveClientFile(ClientFile{
		Boxes:   []Box{{Name: "old", Addr: "old-addr", Token: "old-token"}},
		Current: "old",
	}); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestNestedWriterThroughHomeAliasDoesNotDeadlock$")
	cmd.Env = append(os.Environ(),
		"HOME="+aliasA,
		"PIPER_HOME_ALIAS_A="+aliasA,
		"PIPER_HOME_ALIAS_B="+aliasB,
		"PIPER_HOME_ALIAS_WRITER_HELPER=1",
	)
	if err := cmd.Run(); ctx.Err() == context.DeadlineExceeded {
		t.Fatal("nested writer through a HOME alias deadlocked inside UpdateClientFile callback")
	} else if err != nil {
		t.Fatalf("HOME alias helper failed: %v", err)
	}
}

func runNestedWriterThroughHomeAlias(t *testing.T) {
	aliasA := os.Getenv("PIPER_HOME_ALIAS_A")
	aliasB := os.Getenv("PIPER_HOME_ALIAS_B")
	if aliasA == "" || aliasB == "" {
		t.Fatal("HOME aliases were not provided")
	}
	t.Setenv("HOME", aliasA)
	nestedFile := ClientFile{
		Boxes:   []Box{{Name: "nested", Addr: "nested-addr", Token: "nested-token"}},
		Current: "nested",
	}
	var nestedErr error
	outerErr := UpdateClientFile(func(cf *ClientFile) (bool, error) {
		os.Setenv("HOME", aliasB)
		nestedErr = SaveClientFile(nestedFile)
		os.Setenv("HOME", aliasA)
		cf.Boxes[0].Token = "stale-outer-token"
		return true, nil
	})
	if nestedErr != nil {
		t.Fatalf("nested aliased SaveClientFile failed: %v", nestedErr)
	}
	if !errors.Is(outerErr, ErrClientConfigChanged) {
		t.Fatalf("outer aliased update error = %v, want ErrClientConfigChanged", outerErr)
	}
	os.Setenv("HOME", aliasA)
	got, err := LoadClientFile()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, nestedFile) {
		t.Fatalf("aliased nested writer result was overwritten: got %+v, want %+v", got, nestedFile)
	}
}

func TestConcurrentSameProcessClientWriterDoesNotDeadlock(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := SaveClientFile(ClientFile{
		Boxes:   []Box{{Name: "old", Addr: "old-addr", Token: "old-token"}},
		Current: "old",
	}); err != nil {
		t.Fatal(err)
	}

	started := make(chan struct{})
	release := make(chan struct{})
	outerDone := make(chan error, 1)
	go func() {
		outerDone <- UpdateClientFile(func(cf *ClientFile) (bool, error) {
			close(started)
			<-release
			return true, nil
		})
	}()
	<-started

	writerDone := make(chan error, 1)
	go func() {
		writerDone <- SaveClientFile(ClientFile{
			Boxes:   []Box{{Name: "writer", Addr: "writer-addr", Token: "writer-token"}},
			Current: "writer",
		})
	}()
	select {
	case err := <-writerDone:
		if err != nil {
			t.Fatalf("concurrent writer failed: %v", err)
		}
	case <-time.After(time.Second):
		close(release)
		t.Fatal("concurrent same-process writer did not complete")
	}
	close(release)
	if err := <-outerDone; !errors.Is(err, ErrClientConfigChanged) {
		t.Fatalf("outer update error = %v, want ErrClientConfigChanged", err)
	}
	got, err := LoadClientFile()
	if err != nil {
		t.Fatal(err)
	}
	if got.Current != "writer" || len(got.Boxes) != 1 || got.Boxes[0].Token != "writer-token" {
		t.Fatalf("concurrent writer result was overwritten: %+v", got)
	}
}

func TestClientConfigCallbackErrorReleasesLock(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	original := ClientFile{Boxes: []Box{{Name: "original", Token: "original-token"}}, Current: "original"}
	if err := SaveClientFile(original); err != nil {
		t.Fatal(err)
	}
	wantErr := errors.New("callback failed")
	if err := UpdateClientFile(func(cf *ClientFile) (bool, error) {
		cf.Boxes[0].Token = "discarded"
		return true, wantErr
	}); !errors.Is(err, wantErr) {
		t.Fatalf("callback error = %v, want %v", err, wantErr)
	}
	replacement := ClientFile{Boxes: []Box{{Name: "replacement", Token: "replacement-token"}}, Current: "replacement"}
	if err := SaveClientFile(replacement); err != nil {
		t.Fatalf("writer after callback error failed: %v", err)
	}
	got, err := LoadClientFile()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, replacement) {
		t.Fatalf("writer after callback error got %+v, want %+v", got, replacement)
	}
}

func TestClientConfigStateDoesNotLeakAcrossRoots(t *testing.T) {
	for _, want := range []ClientFile{
		{Boxes: []Box{{Name: "first", Token: "first-token"}}, Current: "first"},
		{Boxes: []Box{{Name: "second", Token: "second-token"}}, Current: "second"},
	} {
		t.Setenv("HOME", t.TempDir())
		if err := SaveClientFile(want); err != nil {
			t.Fatal(err)
		}
		got, err := LoadClientFile()
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("config root leaked state: got %+v, want %+v", got, want)
		}
	}
}

func TestClientConfigLockBlocksOtherProcess(t *testing.T) {
	if os.Getenv("PIPER_CONFIG_LOCK_HOLDER_HELPER") == "1" {
		err := UpdateClientFile(func(cf *ClientFile) (bool, error) {
			_, _ = fmt.Fprintln(os.Stdout, "ready")
			_, err := io.ReadAll(os.Stdin)
			return false, err
		})
		if err != nil {
			t.Fatal(err)
		}
		return
	}
	if os.Getenv("PIPER_CONFIG_LOCK_WRITER_HELPER") == "1" {
		_, _ = fmt.Fprintln(os.Stdout, "ready")
		if err := SaveClientFile(ClientFile{
			Boxes:   []Box{{Name: "other-process", Token: "other-token"}},
			Current: "other-process",
		}); err != nil {
			t.Fatal(err)
		}
		return
	}

	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := SaveClientFile(ClientFile{
		Boxes:   []Box{{Name: "held", Token: "held-token"}},
		Current: "held",
	}); err != nil {
		t.Fatal(err)
	}

	pattern := "^TestClientConfigLockBlocksOtherProcess$"
	holder := exec.Command(os.Args[0], "-test.run="+pattern)
	holder.Env = append(os.Environ(), "HOME="+home, "PIPER_CONFIG_LOCK_HOLDER_HELPER=1")
	holderOut, err := holder.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	holderIn, err := holder.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := holder.Start(); err != nil {
		t.Fatal(err)
	}
	holderDone := make(chan error, 1)
	go func() { holderDone <- holder.Wait() }()
	if _, err := bufio.NewReader(holderOut).ReadString('\n'); err != nil {
		t.Fatal(err)
	}

	writer := exec.Command(os.Args[0], "-test.run="+pattern)
	writer.Env = append(os.Environ(), "HOME="+home, "PIPER_CONFIG_LOCK_WRITER_HELPER=1")
	writerOut, err := writer.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.Start(); err != nil {
		t.Fatal(err)
	}
	writerDone := make(chan error, 1)
	go func() { writerDone <- writer.Wait() }()
	if _, err := bufio.NewReader(writerOut).ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-writerDone:
		t.Fatalf("other process completed while lock was held: %v", err)
	case <-time.After(200 * time.Millisecond):
	}

	if err := holderIn.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-holderDone; err != nil {
		t.Fatalf("lock holder failed: %v", err)
	}
	if err := <-writerDone; err != nil {
		t.Fatalf("other process writer failed after release: %v", err)
	}
	got, err := LoadClientFile()
	if err != nil {
		t.Fatal(err)
	}
	if got.Current != "other-process" || len(got.Boxes) != 1 || got.Boxes[0].Token != "other-token" {
		t.Fatalf("other process writer result missing: %+v", got)
	}
}
