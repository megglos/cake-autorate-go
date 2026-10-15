// SPDX-License-Identifier: GPL-2.0
//
// This file is part of cake-autorate-go, a Go rewrite of cake-autorate.
// Original project: https://github.com/lynxthecat/cake-autorate
// Original author: lynxthecat and contributors
// Licensed under GPL-2.0

package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// The control socket is a Unix socket. A client sends one command as one
// line and reads one line as the answer: "ok" or "error: MESSAGE".
//
//	pause LINK [dl|ul]    pause the rate control (both directions if not set)
//	resume LINK [dl|ul]   resume the rate control
//	list                  answer: the link names, separated by spaces
//
// A pause sets the shaper to the base rate and stops the rate changes. A
// resume starts the control again from the base rate.

// controlTimeout is the time that a client has to send its command.
const controlTimeout = 5 * time.Second

// maxControlLine is the maximum length of a command line in bytes, with the
// newline. The program does not read more from a client.
const maxControlLine = 256

// oneLine replaces the line breaks in s with spaces. An answer must be one
// line, but an error text (for example, the output of tc) can have more.
var oneLine = strings.NewReplacer("\r\n", " ", "\r", " ", "\n", " ")

// LinkControl is the part of a link controller that the control socket uses.
type LinkControl interface {
	SetPaused(dirs []Direction, paused bool) error
}

// handleControlCommand runs one command and returns the answer line.
func handleControlCommand(line string, names []string, links map[string]LinkControl) string {
	f := strings.Fields(line)
	if len(f) == 0 {
		return "error: empty command"
	}
	switch f[0] {
	case "list":
		return strings.Join(names, " ")
	case "pause", "resume":
		if len(f) < 2 || len(f) > 3 {
			return fmt.Sprintf("error: usage: %s LINK [dl|ul]", f[0])
		}
		lc, ok := links[f[1]]
		if !ok {
			return fmt.Sprintf("error: unknown link %q", f[1])
		}
		dirs := []Direction{DL, UL}
		if len(f) == 3 {
			switch f[2] {
			case string(DL):
				dirs = []Direction{DL}
			case string(UL):
				dirs = []Direction{UL}
			default:
				return fmt.Sprintf("error: unknown direction %q, use dl or ul", f[2])
			}
		}
		if err := lc.SetPaused(dirs, f[0] == "pause"); err != nil {
			return "error: " + oneLine.Replace(err.Error())
		}
		return "ok"
	default:
		return fmt.Sprintf("error: unknown command %q", f[0])
	}
}

// removeStaleSocket removes the socket at path if no program listens on it.
// It returns an error if the path is not a socket, or if a program listens
// on it.
func removeStaleSocket(path string) error {
	fi, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if fi.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("control socket path %s exists and is not a socket", path)
	}
	conn, err := net.DialTimeout("unix", path, time.Second)
	if err == nil {
		conn.Close()
		return fmt.Errorf("control socket %s is in use by another program", path)
	}
	// Only a refused connection shows that no program listens. Another
	// error (a full queue, a missing permission, a timeout) can come from a
	// live listener.
	if !errors.Is(err, syscall.ECONNREFUSED) {
		return fmt.Errorf("control socket %s: cannot show that it is stale: %w", path, err)
	}
	return os.Remove(path)
}

// listenPrivate creates a Unix socket at path that only the owner can
// use. It creates the socket in a new directory with mode 0700, sets the
// mode of the socket to 0600 and then moves the socket to path. Thus, no
// other user can connect before the mode is correct. Do not change the
// umask for this: the umask applies to all goroutines of the program.
// listenPrivate returns the listener and the file info of the socket.
func listenPrivate(path string) (net.Listener, os.FileInfo, error) {
	dir, err := os.MkdirTemp(filepath.Dir(path), ".control-")
	if err != nil {
		return nil, nil, err
	}
	defer os.RemoveAll(dir)
	tmp := filepath.Join(dir, "s")
	ln, err := net.Listen("unix", tmp)
	if err != nil {
		return nil, nil, err
	}
	// Do not let the listener remove a path: the socket moves, and another
	// program can replace it by a different file.
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
	if err := os.Chmod(tmp, 0o600); err != nil {
		ln.Close()
		return nil, nil, err
	}
	if err := os.Rename(tmp, path); err != nil {
		ln.Close()
		return nil, nil, err
	}
	created, err := os.Lstat(path)
	if err != nil {
		ln.Close()
		return nil, nil, err
	}
	return ln, created, nil
}

// RunControlSocket listens on the Unix socket at path until ctx ends. Then
// it removes the socket, but only if it is still the socket that this
// function created. Only the owner of the socket can connect.
func RunControlSocket(ctx context.Context, path string, names []string, links map[string]LinkControl, logger *Logger) error {
	if err := removeStaleSocket(path); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	ln, created, err := listenPrivate(path)
	if err != nil {
		return err
	}
	go func() {
		<-ctx.Done()
		ln.Close()
	}()
	// At the return, close the listener (also after an Accept error), then
	// remove the socket, but only our own socket. The defers run in the
	// reverse order.
	defer func() {
		if fi, err := os.Lstat(path); err == nil && os.SameFile(fi, created) {
			os.Remove(path)
		}
	}()
	defer ln.Close()
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			logger.Errorf("control socket: %v", err)
			return err
		}
		go serveControlConn(conn, names, links, logger)
	}
}

// serveControlConn reads one command from conn and writes the answer. It
// runs the command only if the client sent a complete line.
func serveControlConn(conn net.Conn, names []string, links map[string]LinkControl, logger *Logger) {
	defer conn.Close()
	conn.SetReadDeadline(time.Now().Add(controlTimeout))
	// Read not more than maxControlLine bytes. A longer line gives an
	// error, because it has no newline in the limit.
	line, err := bufio.NewReader(io.LimitReader(conn, maxControlLine)).ReadString('\n')
	if err != nil {
		conn.SetWriteDeadline(time.Now().Add(controlTimeout))
		fmt.Fprintln(conn, "error: no complete command")
		return
	}
	answer := handleControlCommand(line, names, links)
	// The answer has its own time. The command can use the read time.
	conn.SetWriteDeadline(time.Now().Add(controlTimeout))
	logger.Infof("control: %q: %s", strings.TrimSpace(line), answer)
	fmt.Fprintln(conn, answer)
}
