// SPDX-License-Identifier: GPL-2.0
//
// cake-autorate-go: A Go rewrite of cake-autorate
//
// This program dynamically adjusts CAKE qdisc bandwidth settings to minimize
// latency on variable-bandwidth connections (LTE, 5G, cable, Starlink, etc.).
//
// Original project: https://github.com/lynxthecat/cake-autorate
// Original author: lynxthecat and contributors
// Licensed under GPL-2.0
//
// This Go rewrite is an experiment to explore how much less resources a native
// implementation may occupy compared to the original bash scripts.

package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

var version = "dev"

func main() {
	if len(os.Args) > 1 && os.Args[1] == "calibrate" {
		os.Exit(runCalibrate(os.Args[2:]))
	}
	configPath := flag.String("config", "/etc/cake-autorate/config.yaml", "path to configuration file")
	showVersion := flag.Bool("version", false, "print version and exit")
	showDefaults := flag.Bool("defaults", false, "print default configuration and exit")
	debug := flag.Bool("debug", false, "enable debug logging (overrides config)")
	recordPath := flag.String("record", "", "write all controller events to this JSONL file (gzip if the name ends with .gz)")
	recordMaxMB := flag.Int("record-max-mb", 0, "stop the recording when the file gets to this size in MB (0: no limit)")
	noAdjust := flag.Bool("no-adjust", false, "do not change the shaper rates (overrides adjust in config); use with --record to measure a link")
	checkConfig := flag.Bool("check-config", false, "check the configuration file, print the warnings and exit (exit code 1 if the file is not valid)")
	strict := flag.Bool("strict", false, "with -check-config: exit with code 1 if the check finds warnings")
	flag.Parse()

	if *showVersion {
		fmt.Printf("cake-autorate-go %s\n", version)
		os.Exit(0)
	}

	if *showDefaults {
		printDefaults()
		os.Exit(0)
	}

	cfg, err := LoadConfig(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	if *checkConfig {
		os.Exit(reportConfigCheck(os.Stdout, os.Stderr, cfg, *strict))
	}

	if *debug {
		cfg.Debug = true
	}
	if *noAdjust {
		for i := range cfg.Links {
			cfg.Links[i].Download.Adjust = false
			cfg.Links[i].Upload.Adjust = false
		}
	}

	logFilePath := cfg.LogFilePath
	if !cfg.LogToFile {
		logFilePath = ""
	}
	logger, err := NewLogger(cfg.Debug, logFilePath, cfg.LogFileMaxSizeKB)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error creating logger: %v\n", err)
		os.Exit(1)
	}
	defer logger.Close()

	logger.Infof("cake-autorate-go %s starting (%d link(s))", version, len(cfg.Links))
	for _, w := range cfg.Warnings() {
		logger.Infof("warning: %s", w)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Handle signals for graceful shutdown
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-sigCh
		logger.Infof("received signal %v, shutting down", sig)
		cancel()
	}()

	// Optional recording of all controller events, for replay and analysis
	var recorder *Recorder
	if *recordPath != "" {
		recorder, err = NewRecorder(*recordPath, int64(*recordMaxMB)*1024*1024, cfg, logger)
		if err != nil {
			logger.Errorf("%v", err)
			os.Exit(1)
		}
		logger.Infof("recording events to %s", *recordPath)
		defer func() {
			if err := recorder.Close(); err != nil {
				logger.Errorf("%v", err)
			}
		}()
	}

	// Shared shaper — uses a persistent netlink socket, safe for concurrent use
	shaper := NewShaper(logger)
	defer shaper.Close()

	names := make([]string, len(cfg.Links))
	for i, l := range cfg.Links {
		names[i] = l.Name
	}

	// Optional status file, for tools that show the state of the links
	var board *StatusBoard
	var wg sync.WaitGroup
	if cfg.StatusFile != "" {
		board = NewStatusBoard(version, time.Now(), names)
		if cfg.StatusHistory {
			board.KeepHistory()
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			RunStatusWriter(ctx, board, cfg.StatusFile, logger)
		}()
	}

	// The controllers stop after a calibration session has restored the
	// rate control (see below). Thus, they have their own context.
	ctrlCtx, ctrlCancel := context.WithCancel(context.Background())
	defer ctrlCancel()

	// Launch a LinkController per configured link
	controls := make(map[string]LinkControl)
	for i := range cfg.Links {
		link := &cfg.Links[i]
		lc := NewLinkController(link, cfg.ForLink(link), shaper, recorder, logger)
		lc.board = board
		controls[link.Name] = lc

		wg.Add(1)
		go func(ctrl *LinkController, linkName string) {
			defer wg.Done()
			if err := ctrl.Run(ctrlCtx); err != nil && ctrlCtx.Err() == nil {
				logger.Errorf("[%s] controller error: %v", linkName, err)
			}
		}(lc, link.Name)
	}

	// Rules of an earlier process (after a crash) go away, also if this
	// config has no control socket. At the stop, the rules of this process
	// go away.
	pinner := NewPinner(cfg.Links, logger)
	pinner.Clear()
	defer func() {
		if err := pinner.Close(); err != nil {
			logger.Errorf("the rules of table %s stay: %v (remove them with \"ip rule del lookup %s\")", pinTable, err, pinTable)
		}
	}()

	var calib *Calibrator
	if cfg.ControlSocket != "" {
		// The calibration sessions write their files next to the
		// control socket (a RAM file system on OpenWrt).
		targets := make(map[string]CalTarget, len(controls))
		for name, lc := range controls {
			targets[name] = lc.(*LinkController)
		}
		calib = NewCalibrator(ctx, filepath.Dir(cfg.ControlSocket), targets, logger)
		calib.probe = probeTimestamps
		calib.pin = pinner
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := RunControlSocket(ctx, cfg.ControlSocket, &ControlHandler{Names: names, Links: controls, History: historySource(board), Calib: calib, Pin: pinner}, logger); err != nil {
				logger.Errorf("control socket: %v", err)
			}
		}()
	}

	// At the stop, a calibration session ends first: it restores the rate
	// control through the controllers. Then the controllers stop.
	go func() {
		<-ctx.Done()
		calib.Wait()
		ctrlCancel()
	}()

	wg.Wait()
	logger.Infof("cake-autorate-go stopped")
}

// reportConfigCheck prints the warnings of a valid config and returns the
// exit code of -check-config. With strict, a warning gives exit code 1.
func reportConfigCheck(out, errOut io.Writer, cfg *Config, strict bool) int {
	warnings := cfg.Warnings()
	for _, w := range warnings {
		fmt.Fprintf(errOut, "warning: %s\n", w)
	}
	if strict && len(warnings) > 0 {
		return 1
	}
	fmt.Fprintf(out, "config is valid: %d link(s)\n", len(cfg.Links))
	return 0
}

func printDefaults() {
	cfg := DefaultConfig()
	fmt.Println("# cake-autorate-go default configuration")
	fmt.Println("# Save this to /etc/cake-autorate/config.yaml and adjust as needed")
	fmt.Println()
	fmt.Println("# ── Link Configuration ─────────────────────────────────────────────────────")
	fmt.Println("# Each link represents a WAN connection with its own download/upload interfaces")
	fmt.Println("# and reflectors. Multiple links run concurrently in a single process.")
	fmt.Println("#")
	fmt.Println("# For a single WAN, you can also use the legacy flat format (download:/upload:")
	fmt.Println("# at top level) — it will be auto-migrated to a single link named 'default'.")
	fmt.Println()
	fmt.Println("links:")
	fmt.Println("  - name: primary")
	fmt.Printf("    download:\n")
	fmt.Printf("      interface: %s\n", cfg.Download.Interface)
	fmt.Printf("      adjust: %t\n", cfg.Download.Adjust)
	fmt.Printf("      min_rate_kbps: %d\n", cfg.Download.MinRateKbps)
	fmt.Printf("      base_rate_kbps: %d\n", cfg.Download.BaseRateKbps)
	fmt.Printf("      max_rate_kbps: %d\n", cfg.Download.MaxRateKbps)
	fmt.Printf("      owd_delta_delay_thr_ms: %.1f\n", cfg.Download.OWDDeltaDelayThrMs)
	fmt.Printf("      avg_owd_delta_max_adjust_up_thr_ms: %.1f\n", cfg.Download.AvgOWDDeltaMaxAdjUpThrMs)
	fmt.Printf("      avg_owd_delta_max_adjust_down_thr_ms: %.1f\n", cfg.Download.AvgOWDDeltaMaxAdjDownThrMs)
	fmt.Printf("    upload:\n")
	fmt.Printf("      interface: %s\n", cfg.Upload.Interface)
	fmt.Printf("      adjust: %t\n", cfg.Upload.Adjust)
	fmt.Printf("      min_rate_kbps: %d\n", cfg.Upload.MinRateKbps)
	fmt.Printf("      base_rate_kbps: %d\n", cfg.Upload.BaseRateKbps)
	fmt.Printf("      max_rate_kbps: %d\n", cfg.Upload.MaxRateKbps)
	fmt.Printf("      owd_delta_delay_thr_ms: %.1f\n", cfg.Upload.OWDDeltaDelayThrMs)
	fmt.Printf("      avg_owd_delta_max_adjust_up_thr_ms: %.1f\n", cfg.Upload.AvgOWDDeltaMaxAdjUpThrMs)
	fmt.Printf("      avg_owd_delta_max_adjust_down_thr_ms: %.1f\n", cfg.Upload.AvgOWDDeltaMaxAdjDownThrMs)
	fmt.Printf("    reflectors:\n")
	for _, r := range cfg.Reflectors {
		fmt.Printf("      - %s\n", r)
	}
	fmt.Printf("    # ping_interface: wan         # Bind ICMP socket to this interface\n")
	fmt.Printf("    # ping_source_addr: 0.0.0.0  # Source IP for ICMP packets\n")

	fmt.Printf("\n# ── Shared Settings ────────────────────────────────────────────────────────\n")
	fmt.Printf("# These apply to all links.\n\n")
	fmt.Printf("pinger_count: %d\n", cfg.PingerCount)
	fmt.Printf("ping_interval_ms: %d\n", cfg.PingIntervalMs)
	fmt.Printf("\nenable_sleep_function: %t\n", cfg.EnableSleepFunction)
	fmt.Printf("connection_active_thr_kbps: %d\n", cfg.ConnectionActiveThrKbps)
	fmt.Printf("sustained_idle_sleep_thr_s: %.1f\n", cfg.SustainedIdleSleepThrS)
	fmt.Printf("\nbufferbloat_detection_window: %d\n", cfg.BufferbloatDetectionWindow)
	fmt.Printf("bufferbloat_detection_thr: %d\n", cfg.BufferbloatDetectionThr)
	fmt.Printf("\nalpha_baseline_increase: %g\n", cfg.AlphaBaselineIncrease)
	fmt.Printf("alpha_baseline_decrease: %g\n", cfg.AlphaBaselineDecrease)
	fmt.Printf("alpha_delta_ewma: %g\n", cfg.AlphaDeltaEWMA)
	fmt.Printf("\nshaper_rate_min_adjust_down_bufferbloat: %g\n", cfg.ShaperRateMinAdjustDownBufferbloat)
	fmt.Printf("shaper_rate_max_adjust_down_bufferbloat: %g\n", cfg.ShaperRateMaxAdjustDownBufferbloat)
	fmt.Printf("shaper_rate_min_adjust_up_load_high: %g\n", cfg.ShaperRateMinAdjustUpLoadHigh)
	fmt.Printf("shaper_rate_max_adjust_up_load_high: %g\n", cfg.ShaperRateMaxAdjustUpLoadHigh)
	fmt.Printf("shaper_rate_adjust_down_load_low: %g\n", cfg.ShaperRateAdjustDownLoadLow)
	fmt.Printf("shaper_rate_adjust_up_load_low: %g\n", cfg.ShaperRateAdjustUpLoadLow)
	fmt.Printf("\nhigh_load_thr: %g\n", cfg.HighLoadThr)
	fmt.Printf("\nmonitor_interval_ms: %d\n", cfg.MonitorIntervalMs)
	fmt.Printf("bufferbloat_refractory_period_ms: %d\n", cfg.BufferbloatRefractoryPeriodMs)
	fmt.Printf("refractory_on_decrease: %v\n", cfg.RefractoryOnDecrease)
	fmt.Printf("repeat_decrease_on_rise: %v\n", cfg.RepeatDecreaseOnRise)
	fmt.Printf("decay_refractory_period_ms: %d\n", cfg.DecayRefractoryPeriodMs)
	fmt.Printf("stall_recovery_ms: %d\n", cfg.StallRecoveryMs)
	fmt.Printf("learn_cross_delay: %v\n", cfg.LearnCrossDelay)
	fmt.Printf("increase_to_delay_thr: %v\n", cfg.IncreaseToDelayThr)
	fmt.Printf("auto_max_rate: %v\n", cfg.AutoMaxRate)
	fmt.Printf("auto_max_rate_limit_factor: %g\n", cfg.AutoMaxRateLimitFactor)
	fmt.Printf("\nreflector_response_deadline_s: %g\n", cfg.ReflectorResponseDeadlineS)
	fmt.Printf("reflector_misbehaving_detection_window: %d\n", cfg.ReflectorMisbehavingDetectionWindow)
	fmt.Printf("reflector_misbehaving_detection_thr: %d\n", cfg.ReflectorMisbehavingDetectionThr)
	fmt.Printf("\nstall_detection_thr: %d\n", cfg.StallDetectionThr)
	fmt.Printf("connection_stall_thr_kbps: %d\n", cfg.ConnectionStallThrKbps)
	fmt.Printf("global_ping_response_timeout_s: %g\n", cfg.GlobalPingResponseTimeoutS)
	fmt.Printf("\nmin_shaper_rates_enforcement: %t\n", cfg.MinShaperRatesEnforcement)
	fmt.Printf("startup_wait_s: %g\n", cfg.StartupWaitS)
	fmt.Printf("\nlog_to_file: %t\n", cfg.LogToFile)
	fmt.Printf("log_file_path: %s\n", cfg.LogFilePath)
	fmt.Printf("log_file_max_size_kb: %d\n", cfg.LogFileMaxSizeKB)
	fmt.Printf("debug: %t\n", cfg.Debug)
	fmt.Printf("# status_file: /var/run/cake-autorate/status.json  # status of all links, written each second\n")
	fmt.Printf("# control_socket: /var/run/cake-autorate/control.sock  # commands: pause, resume, list\n")
}
