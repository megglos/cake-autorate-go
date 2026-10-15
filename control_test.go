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
	// An error text with line breaks (tc output) gives one answer line.
	a.err = fmt.Errorf("tc failed:\nError: line 1\r\nline 2")
	if got := handleControlCommand("pause a", names, links); got != "error: tc failed: Error: line 1 line 2" {
		t.Errorf("multiline error: %q", got)
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

func TestRunControlSocket_RefusesLiveSocket(t *testing.T) {
	path := filepath.Join(t.TempDir(), "control.sock")
	live, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer live.Close()
	if err := RunControlSocket(context.Background(), path, nil, nil, NewDiscardLogger()); err == nil || !strings.Contains(err.Error(), "in use") {
		t.Errorf("expected an in-use error, got: %v", err)
	}
	if _, err := os.Lstat(path); err != nil {
		t.Error("the live socket must stay")
	}
}

func TestRunControlSocket_ReplacesStaleSocket(t *testing.T) {
	path := filepath.Join(t.TempDir(), "control.sock")
	stale, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	// Close the listener without removal: the socket file stays.
	stale.(*net.UnixListener).SetUnlinkOnClose(false)
	stale.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- RunControlSocket(ctx, path, nil, nil, NewDiscardLogger()) }()
	var conn net.Conn
	for range 50 {
		if conn, err = net.Dial("unix", path); err == nil {
			conn.Close()
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("no listener on the stale path: %v", err)
	}
	cancel()
	<-done
}

func TestRunControlSocket_KeepsReplacementAtStop(t *testing.T) {
	path := filepath.Join(t.TempDir(), "control.sock")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- RunControlSocket(ctx, path, nil, nil, NewDiscardLogger()) }()
	for range 50 {
		if c, err := net.Dial("unix", path); err == nil {
			c.Close()
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	// Another program replaces the socket by a regular file.
	os.Remove(path)
	if err := os.WriteFile(path, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	cancel()
	<-done
	if data, err := os.ReadFile(path); err != nil || string(data) != "keep" {
		t.Errorf("the replacement file must stay: %q %v", data, err)
	}
}

func TestServeControlConn_IgnoresIncompleteCommand(t *testing.T) {
	a := &fakeLinkControl{}
	client, server := net.Pipe()
	go func() {
		serveControlConn(server, []string{"a"}, map[string]LinkControl{"a": a}, NewDiscardLogger())
	}()
	fmt.Fprint(client, "pause a") // no newline
	client.Close()
	time.Sleep(100 * time.Millisecond)
	if a.dirs != nil || a.paused {
		t.Errorf("an incomplete command must not run: %v %v", a.dirs, a.paused)
	}
}

func TestRemoveStaleSocket_OtherErrorsKeepSocket(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "control.sock")
	// A socket file that is not accessible gives a permission error, not
	// ECONNREFUSED. The test needs a non-root user.
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	l.(*net.UnixListener).SetUnlinkOnClose(false)
	l.Close()
	if err := os.Chmod(path, 0); err != nil {
		t.Fatal(err)
	}
	if os.Geteuid() == 0 {
		t.Skip("root ignores the file mode")
	}
	if err := removeStaleSocket(path); err == nil {
		t.Error("a probe error other than ECONNREFUSED must not remove the socket")
	}
	if _, err := os.Lstat(path); err != nil {
		t.Error("the socket must stay")
	}
}

func TestServeControlConn_RejectsLongLine(t *testing.T) {
	a := &fakeLinkControl{}
	client, server := net.Pipe()
	go serveControlConn(server, []string{"a"}, map[string]LinkControl{"a": a}, NewDiscardLogger())
	go func() {
		// Write more than the limit, then a valid command. The program must
		// stop the read at the limit and not run the command.
		client.Write([]byte(strings.Repeat("x", maxControlLine) + "\npause a\n"))
	}()
	answer, _ := bufio.NewReader(client).ReadString('\n')
	client.Close()
	if !strings.HasPrefix(answer, "error: no complete command") || a.paused {
		t.Errorf("answer %q, paused %v", answer, a.paused)
	}
}
