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
	"fmt"
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
			return "error: " + err.Error()
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
	if conn, err := net.DialTimeout("unix", path, time.Second); err == nil {
		conn.Close()
		return fmt.Errorf("control socket %s is in use by another program", path)
	}
	return os.Remove(path)
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
	// Create the socket with mode 0600 from the start. A mode change after
	// the creation leaves a short time in which other users can connect.
	old := syscall.Umask(0o177)
	ln, err := net.Listen("unix", path)
	syscall.Umask(old)
	if err != nil {
		return err
	}
	// Do not let the listener remove the path: another program can have
	// replaced the socket by a different file.
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
	created, err := os.Lstat(path)
	if err != nil {
		ln.Close()
		return err
	}
	go func() {
		<-ctx.Done()
		ln.Close()
	}()
	// Remove the socket before the return, but only our own socket.
	defer func() {
		if fi, err := os.Lstat(path); err == nil && os.SameFile(fi, created) {
			os.Remove(path)
		}
	}()
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
	conn.SetDeadline(time.Now().Add(controlTimeout))
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		fmt.Fprintln(conn, "error: no complete command")
		return
	}
	answer := handleControlCommand(line, names, links)
	logger.Infof("control: %q: %s", strings.TrimSpace(line), answer)
	fmt.Fprintln(conn, answer)
}
