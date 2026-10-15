package main

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

type fakeLinkControl struct {
	dirs   []Direction
	paused bool
	err    error
}

func (f *fakeLinkControl) SetPaused(dirs []Direction, paused bool) error {
	f.dirs, f.paused = dirs, paused
	return f.err
}

func TestHandleControlCommand(t *testing.T) {
	a := &fakeLinkControl{}
	links := map[string]LinkControl{"a": a}
	names := []string{"a", "b"}
	for _, tc := range []struct {
		line, want string
		dirs       []Direction
		paused     bool
	}{
		{"list", "a b", nil, false},
		{"pause a", "ok", []Direction{DL, UL}, true},
		{"resume a ul", "ok", []Direction{UL}, false},
		{"pause a dl\n", "ok", []Direction{DL}, true},
		{"pause x", `error: unknown link "x"`, nil, false},
		{"pause a sideways", "error: unknown direction", nil, false},
		{"pause", "error: usage", nil, false},
		{"", "error: empty", nil, false},
		{"stop a", "error: unknown command", nil, false},
	} {
		*a = fakeLinkControl{}
		got := handleControlCommand(tc.line, names, links)
		if !strings.HasPrefix(got, tc.want) {
			t.Errorf("%q: got %q, want prefix %q", tc.line, got, tc.want)
		}
		if !slices.Equal(a.dirs, tc.dirs) || a.paused != tc.paused {
			t.Errorf("%q: link got dirs %v paused %v", tc.line, a.dirs, a.paused)
		}
	}
	a.err = fmt.Errorf("link does not run")
	if got := handleControlCommand("pause a", names, links); got != "error: link does not run" {
		t.Errorf("error from the link: %q", got)
	}
}

func TestRunControlSocket(t *testing.T) {
	path := filepath.Join(t.TempDir(), "run", "control.sock")
	a := &fakeLinkControl{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- RunControlSocket(ctx, path, []string{"a"}, map[string]LinkControl{"a": a}, NewDiscardLogger())
	}()

	var conn net.Conn
	var err error
	for range 50 {
		if conn, err = net.Dial("unix", path); err == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(path)
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("socket mode: %v", fi.Mode())
	}
	fmt.Fprintln(conn, "pause a dl")
	answer, _ := bufio.NewReader(conn).ReadString('\n')
	conn.Close()
	if strings.TrimSpace(answer) != "ok" || !a.paused {
		t.Errorf("answer %q, paused %v", answer, a.paused)
	}

	cancel()
	if err := <-done; err != nil {
		t.Error(err)
	}
	if _, err := os.Stat(path); err == nil {
		t.Error("socket must be removed at stop")
	}
}

func TestRunControlSocket_RefusesRegularFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "file")
	os.WriteFile(path, []byte("x"), 0o600)
	err := RunControlSocket(context.Background(), path, nil, nil, NewDiscardLogger())
	if err == nil {
		t.Error("expected an error for a path that is a regular file")
	}
	if _, err := os.Stat(path); err != nil {
		t.Error("the file must stay")
	}
}
