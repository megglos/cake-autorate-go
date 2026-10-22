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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// The control socket is a Unix socket. A client sends one command as one
// line and reads one line as the answer: "ok" or "error: MESSAGE".
//
//	pause LINK [dl|ul]    pause the rate control (both directions if not set)
//	resume LINK [dl|ul]   resume the rate control
//	unshape LINK [SECONDS] turn the shaping of the link off (for SECONDS,
//	                      1 to 3600; without SECONDS until "shape")
//	shape LINK            turn the shaping of the link on again
//	mode LINK MODE [SECONDS] set the link mode (latency, balanced,
//	                      bandwidth or custom; for SECONDS, 1 to 86400,
//	                      then back to the mode of the config)
//	list                  answer: the link names, separated by spaces
//	history               answer: the samples of the last 15 minutes of
//	                      each link as one line of JSON (see History)
//	calibrate start LINK [LOAD_S]
//	                      start a calibration session for the link (see
//	                      Calibrator). LOAD_S: the load time of each block
//	calibrate status      answer: the status of the session as one line
//	                      of JSON (see CalStatus)
//	calibrate stop [SESSION] stop the session (only the session SESSION)
//	pin LINK SECONDS      send the traffic to the load hosts of the LuCI
//	                      app through the link (1 to 3600 s, see Pinner).
//	                      Answer: "ok ID"
//	unpin [LINK [ID]]     end the pin (of the link; with ID only that pin)
//
// A pause sets the shaper to the base rate and stops the rate changes. A
// resume starts the control again from the base rate.
//
// Without shaping, the shaper rate is 10 Gbit/s (no useful limit) and the
// rate control stops. The program does not keep this state: at a restart
// and before a stop, it shapes the link again.

// maxUnshapeSeconds is the maximum time of "unshape LINK SECONDS".
const maxUnshapeSeconds = 3600

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
	SetUnshaped(on bool, d time.Duration) error
	SetMode(m LinkMode, d time.Duration) error
}

// PinControl is the part of the Pinner that the control socket and the
// Calibrator use. owner is PinOwnerControl or PinOwnerCalibration.
type PinControl interface {
	Pin(owner, link string, d time.Duration) (int, error)
	Unpin(owner, link string, id int) error
	Extend(owner, link string, id int, d time.Duration) error
}

// CalControl is the part of the Calibrator that the control socket uses.
type CalControl interface {
	Start(link string, times calTimes) error
	Stop(session string) error
	Status() CalStatus
}

// ControlHandler has what the commands of the control socket use. History,
// Calib and Pin can be nil.
type ControlHandler struct {
	Names   []string
	Links   map[string]LinkControl
	History HistorySource
	Calib   CalControl
	Pin     PinControl
}

// handleControlCommand runs one command and returns the answer line.
func handleControlCommand(line string, h *ControlHandler) string {
	names, links, history := h.Names, h.Links, h.History
	f := strings.Fields(line)
	if len(f) == 0 {
		return "error: empty command"
	}
	switch f[0] {
	case "list":
		return strings.Join(names, " ")
	case "history":
		if len(f) != 1 {
			return "error: usage: history"
		}
		if history == nil {
			return "error: no history"
		}
		data, err := history.HistoryJSON()
		if err != nil {
			return "error: " + oneLine.Replace(err.Error())
		}
		return string(data)
	case "calibrate":
		return handleCalibrateCommand(f[1:], h)
	case "pin", "unpin":
		return handlePinCommand(f, h)
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
	case "unshape", "shape":
		usage := "error: usage: shape LINK"
		if f[0] == "unshape" {
			usage = "error: usage: unshape LINK [SECONDS]"
		}
		if len(f) < 2 || len(f) > 3 || (f[0] == "shape" && len(f) != 2) {
			return usage
		}
		var d time.Duration
		if len(f) == 3 {
			// Check the range before the conversion to a duration.
			s, err := strconv.Atoi(f[2])
			if err != nil || s < 1 || s > maxUnshapeSeconds {
				return fmt.Sprintf("error: SECONDS must be 1 to %d", maxUnshapeSeconds)
			}
			d = time.Duration(s) * time.Second
		}
		lc, ok := links[f[1]]
		if !ok {
			return fmt.Sprintf("error: unknown link %q", f[1])
		}
		if err := lc.SetUnshaped(f[0] == "unshape", d); err != nil {
			return "error: " + oneLine.Replace(err.Error())
		}
		return "ok"
	case "mode":
		if len(f) < 3 || len(f) > 4 {
			return "error: usage: mode LINK latency|balanced|bandwidth|custom [SECONDS]"
		}
		m, err := parseLinkMode(f[2])
		if err != nil {
			return "error: " + err.Error()
		}
		var d time.Duration
		if len(f) == 4 {
			// Check the range before the conversion to a duration.
			s, err := strconv.Atoi(f[3])
			if err != nil || s < 1 || s > maxModeSeconds {
				return fmt.Sprintf("error: SECONDS must be 1 to %d", maxModeSeconds)
			}
			d = time.Duration(s) * time.Second
		}
		lc, ok := links[f[1]]
		if !ok {
			return fmt.Sprintf("error: unknown link %q", f[1])
		}
		if err := lc.SetMode(m, d); err != nil {
			return "error: " + oneLine.Replace(err.Error())
		}
		return "ok"
	default:
		return fmt.Sprintf("error: unknown command %q", f[0])
	}
}

// handlePinCommand runs "pin LINK SECONDS" and "unpin [LINK]".
func handlePinCommand(f []string, h *ControlHandler) string {
	if h.Pin == nil {
		return "error: " + errNoPinner.Error()
	}
	var err error
	if f[0] == "pin" {
		if len(f) != 3 {
			return "error: usage: pin LINK SECONDS"
		}
		// Check the range before the conversion to a duration.
		s, convErr := strconv.Atoi(f[2])
		if convErr != nil || s < 1 || s > maxPinSeconds {
			return fmt.Sprintf("error: SECONDS must be 1 to %d", maxPinSeconds)
		}
		id, err := h.Pin.Pin(PinOwnerControl, f[1], time.Duration(s)*time.Second)
		if err != nil {
			return "error: " + oneLine.Replace(err.Error())
		}
		return fmt.Sprintf("ok %d", id)
	} else {
		if len(f) > 3 {
			return "error: usage: unpin [LINK [ID]]"
		}
		link, id := "", 0
		if len(f) >= 2 {
			link = f[1]
		}
		if len(f) == 3 {
			n, convErr := strconv.Atoi(f[2])
			if convErr != nil || n < 1 {
				return "error: ID must be a positive number"
			}
			id = n
		}
		err = h.Pin.Unpin(PinOwnerControl, link, id)
	}
	if err != nil {
		return "error: " + oneLine.Replace(err.Error())
	}
	return "ok"
}

// handleCalibrateCommand runs "calibrate start|status|stop".
func handleCalibrateCommand(args []string, h *ControlHandler) string {
	if h.Calib == nil {
		return "error: no calibration"
	}
	if len(args) == 0 {
		return "error: usage: calibrate start LINK [LOAD_S] | status | stop"
	}
	switch args[0] {
	case "start":
		if len(args) < 2 || len(args) > 3 {
			return "error: usage: calibrate start LINK [LOAD_S]"
		}
		load := calDefaultLoad
		if len(args) == 3 {
			// Check the seconds before the conversion: a large value
			// overflows the multiplication.
			s, err := strconv.Atoi(args[2])
			if err != nil || s < int(calMinLoad/time.Second) || s > int(calMaxLoad/time.Second) {
				return fmt.Sprintf("error: load time %q: use %d to %d seconds", args[2], int(calMinLoad.Seconds()), int(calMaxLoad.Seconds()))
			}
			load = time.Duration(s) * time.Second
		}
		if err := h.Calib.Start(args[1], defaultCalTimes(load)); err != nil {
			return "error: " + oneLine.Replace(err.Error())
		}
		return "ok"
	case "status", "stop":
		if len(args) != 1 && !(args[0] == "stop" && len(args) == 2) {
			if args[0] == "stop" {
				return "error: usage: calibrate stop [SESSION]"
			}
			return fmt.Sprintf("error: usage: calibrate %s", args[0])
		}
		if args[0] == "stop" {
			session := ""
			if len(args) == 2 {
				session = args[1]
			}
			if err := h.Calib.Stop(session); err != nil {
				return "error: " + oneLine.Replace(err.Error())
			}
			return "ok"
		}
		data, err := json.Marshal(h.Calib.Status())
		if err != nil {
			return "error: " + oneLine.Replace(err.Error())
		}
		return string(data)
	default:
		return fmt.Sprintf("error: unknown calibrate command %q", args[0])
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
// The name of the directory is short, because Linux limits the length of a
// socket path (see maxControlSocketPath). listenPrivate returns the
// listener and the file info of the socket.
func listenPrivate(path string) (net.Listener, os.FileInfo, error) {
	dir, err := os.MkdirTemp(filepath.Dir(path), ".c")
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
func RunControlSocket(ctx context.Context, path string, h *ControlHandler, logger *Logger) error {
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
		go serveControlConn(conn, h, logger)
	}
}

// serveControlConn reads one command from conn and writes the answer. It
// runs the command only if the client sent a complete line.
func serveControlConn(conn net.Conn, h *ControlHandler, logger *Logger) {
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
	answer := handleControlCommand(line, h)
	// The answer has its own time. The command can use the read time.
	conn.SetWriteDeadline(time.Now().Add(controlTimeout))
	// The history is long and a page reads it at each load. A page reads
	// the calibration status each second. Log only the size, as debug.
	if cmd := strings.TrimSpace(line); (cmd == "history" || cmd == "calibrate status") && !strings.HasPrefix(answer, "error: ") {
		logger.Debugf("control: %q: %d bytes", cmd, len(answer))
	} else {
		logger.Infof("control: %q: %s", cmd, answer)
	}
	fmt.Fprintln(conn, answer)
}
