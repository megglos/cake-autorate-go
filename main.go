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
	"os"
	"os/signal"
	"sync"
	"syscall"
)

var version = "dev"

func main() {
	configPath := flag.String("config", "/etc/cake-autorate/config.yaml", "path to configuration file")
	showVersion := flag.Bool("version", false, "print version and exit")
	showDefaults := flag.Bool("defaults", false, "print default configuration and exit")
	debug := flag.Bool("debug", false, "enable debug logging (overrides config)")
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

	if *debug {
		cfg.Debug = true
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

	// Shared shaper — uses a persistent netlink socket, safe for concurrent use
	shaper := NewShaper(logger)
	defer shaper.Close()

	// Launch a LinkController per configured link
	var wg sync.WaitGroup
	for i := range cfg.Links {
		link := &cfg.Links[i]
		lc := NewLinkController(link, cfg, shaper, logger)

		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := lc.Run(ctx); err != nil && ctx.Err() == nil {
				logger.Errorf("[%s] controller error: %v", link.Name, err)
			}
		}()
	}

	wg.Wait()
	logger.Infof("cake-autorate-go stopped")
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
	fmt.Printf("decay_refractory_period_ms: %d\n", cfg.DecayRefractoryPeriodMs)
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
}
