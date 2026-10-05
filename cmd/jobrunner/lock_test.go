package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"testing"
	"time"
)

func TestJobLockHelper(t *testing.T) {
	if os.Getenv("JOBRUNNER_LOCK_HELPER") != "1" {
		return
	}
	fmt.Println("ready")
	release, err := lockJob(os.Getenv("JOBRUNNER_LOCK_ROOT"), "shared")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	fmt.Println("acquired")
	if _, err := io.Copy(io.Discard, os.Stdin); err != nil {
		t.Fatal(err)
	}
}

func TestJobLockSerializesProcesses(t *testing.T) {
	root := t.TempDir()
	release, err := lockJob(root, "shared")
	if err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	unlock := func() { once.Do(release) }
	defer unlock()

	// Different jobs must remain independent while this job is locked.
	otherRelease, err := lockJob(root, "other")
	if err != nil {
		t.Fatal(err)
	}
	otherRelease()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestJobLockHelper$")
	cmd.Env = append(os.Environ(), "JOBRUNNER_LOCK_HELPER=1", "JOBRUNNER_LOCK_ROOT="+root)
	cmd.Stderr = os.Stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cancel()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})

	lines := make(chan string, 4)
	go func() {
		defer close(lines)
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			lines <- scanner.Text()
		}
	}()
	expect := func(want string) {
		t.Helper()
		select {
		case got, ok := <-lines:
			if !ok || got != want {
				t.Fatalf("helper output = %q; want %q", got, want)
			}
		case <-ctx.Done():
			t.Fatalf("waiting for %q: %v", want, ctx.Err())
		}
	}

	expect("ready")
	select {
	case got := <-lines:
		t.Fatalf("helper did not wait for the job lock: %q", got)
	case <-time.After(200 * time.Millisecond):
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	unlock()
	expect("acquired")
	if err := stdin.Close(); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
}
