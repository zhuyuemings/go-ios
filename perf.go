package main

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/danielpaulus/go-ios/ios"
	"github.com/danielpaulus/go-ios/ios/instruments"
	log "github.com/sirupsen/logrus"
)

type perfJsonOutput struct {
	Type       string   `json:"type"`
	Timestamp  uint64   `json:"timestamp"`
	BundleID   string   `json:"bundle_id,omitempty"`
	PID        uint64   `json:"pid,omitempty"`
	FPS        *float64 `json:"fps,omitempty"`
	CPUUsage   *float64 `json:"cpu_usage,omitempty"`
	MemUsage   *float64 `json:"mem_usage,omitempty"`
	NetInKbps  *float64 `json:"net_in_kbps,omitempty"`
	NetOutKbps *float64 `json:"net_out_kbps,omitempty"`
}

func runPerfCommand(device ios.DeviceEntry, bundleID string, humanReadable bool) {
	var targetPID uint64 = 0

	// Resolve PID if bundleID is provided
	if bundleID != "" {
		deviceInfoService, err := instruments.NewDeviceInfoService(device)
		if err != nil {
			log.Fatalf("failed to connect to device info service: %v", err)
		}

		procs, err := deviceInfoService.ProcessList()
		deviceInfoService.Close()
		if err != nil {
			log.Fatalf("failed to get process list: %v", err)
		}

		found := false
		for _, p := range procs {
			// Instruments runningProcesses name might not be exactly bundleID,
			// but we will match by Name or RealAppName or exact bundleID.
			// Ideally we'd use installation proxy to find the executable name,
			// but checking against process name is a common heuristic.
			if p.Name == bundleID || p.RealAppName == bundleID {
				targetPID = p.Pid
				found = true
				break
			}
		}

		if !found {
			log.Fatalf("could not find running process for bundle ID or name: %s", bundleID)
		}

		log.Debugf("Resolved target PID for %s: %d", bundleID, targetPID)
	}

	// 1. Start Sysmontap
	// Default sampling rate 10 (same as xcode / ios sysmontap command)
	sysmon, err := instruments.NewSysmontapService(device, 10)
	if err != nil {
		log.Fatalf("failed to start sysmontap service: %v", err)
	}
	defer sysmon.Close()
	sysmonChan := sysmon.ReceiveMessage()

	// 1.5 Start Network Monitor
	netMonitor, err := instruments.NewNetworkMonitorService(device)
	if err != nil {
		log.Fatalf("failed to start network monitor service: %v", err)
	}
	defer netMonitor.Close()
	err = netMonitor.StartMonitoring()
	if err != nil {
		log.Fatalf("failed to start network monitoring: %v", err)
	}
	netAggregator := netMonitor.GetAggregator()

	// 2. Start Graphics (FPS)
	graphics, err := instruments.NewGraphicsService(device)
	if err != nil {
		log.Fatalf("failed to start graphics service: %v", err)
	}
	defer graphics.Close()
	fpsChan := graphics.ReceiveFPS()

	// 3. Handle interrupts for clean shutdown
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-c
		log.Info("shutting down perf monitoring")
		sysmon.Close()
		netMonitor.Close()
		graphics.Close()
		os.Exit(0)
	}()

	if humanReadable {
		fmt.Printf("Starting performance monitoring... (Target: %s)\n", bundleID)
		fmt.Printf("%-20s %-10s %-15s %-15s %-10s\n", "TIME", "TYPE", "CPU (%)", "MEM (MB)", "FPS")
		fmt.Println("-----------------------------------------------------------------------")
	}

	var (
		firstNet = true
		lastNetIn, lastNetOut, lastTs uint64
	)
	
	for {
		select {
		case msg, ok := <-sysmonChan:
			if !ok {
				return
			}
			ts := uint64(time.Now().UnixMilli())
			if msg.EndMachAbsTime > 0 {
				ts = msg.EndMachAbsTime
			}

			var cpu float64
			var mem uint64

			if targetPID != 0 {
				if procData, exists := msg.Processes[targetPID]; exists {
					cpu = procData.CPUUsage
					mem = procData.PhysFootprint
				} else {
					// Process not found in this sample, it might have exited
					continue
				}
			} else {
				cpu = msg.SystemCPUUsage.CPU_TotalLoad

				var totalMem uint64
				for _, procData := range msg.Processes {
					totalMem += procData.PhysFootprint
				}

				if sysMem, ok := msg.SystemMemory["phys_footprint"]; ok {
					if v, valid := instruments.ToUint64(sysMem); valid {
						totalMem = v
					}
				} else if sysMem, ok := msg.SystemMemory["Physical Footprint"]; ok {
					if v, valid := instruments.ToUint64(sysMem); valid {
						totalMem = v
					}
				} else if sysMem, ok := msg.SystemMemory["Memory"]; ok {
					if v, valid := instruments.ToUint64(sysMem); valid {
						totalMem = v
					}
				}

				mem = totalMem
			}

			if msg.CPUCount > 0 {
				cpu = cpu / float64(msg.CPUCount)
			}
			cpu = math.Round(cpu*100) / 100

			var memUsage float64
			var physMemSize uint64
			if sizeVal, ok := msg.SystemMemory["mem_size"]; ok {
				if v, valid := instruments.ToUint64(sizeVal); valid {
					physMemSize = v
				}
			}
			if physMemSize > 0 {
				memUsage = (float64(mem) / float64(physMemSize)) * 100.0
				if memUsage > 100.0 {
					memUsage = 100.0
				}
				memUsage = math.Round(memUsage*100) / 100
			}

			var netInPtr, netOutPtr *float64

			if humanReadable {
				memMB := float64(mem) / 1024 / 1024
				fmt.Printf("%-20s %-10s %-15.2f %-15.2f %-10s\n", time.Now().Format("15:04:05.000"), "SYSMON", cpu, memMB, "-")
			} else {
				// Query NetworkMonitorAggregator for pure network bytes!
				netInBytes, netOutBytes := netAggregator.GetNetworkBytes(targetPID)

				var deltaNetIn, deltaNetOut uint64
				if firstNet {
					firstNet = false
					deltaNetIn = 0
					deltaNetOut = 0
				} else {
					if netInBytes >= lastNetIn {
						deltaNetIn = netInBytes - lastNetIn
					}
					if netOutBytes >= lastNetOut {
						deltaNetOut = netOutBytes - lastNetOut
					}
				}

				var netInKbps, netOutKbps float64
				if !firstNet && lastTs > 0 {
					deltaMs := ts - lastTs
					if deltaMs > 0 {
						if deltaNetIn > 0 {
							netInKbps = math.Round((float64(deltaNetIn)/1024.0)/(float64(deltaMs)/1000.0)*100) / 100
						}
						if deltaNetOut > 0 {
							netOutKbps = math.Round((float64(deltaNetOut)/1024.0)/(float64(deltaMs)/1000.0)*100) / 100
						}
					}
				}

				lastNetIn = netInBytes
				lastNetOut = netOutBytes
				lastTs = uint64(ts)

				if netInKbps > 0 { netInPtr = &netInKbps }
				if netOutKbps > 0 { netOutPtr = &netOutKbps }

				out := perfJsonOutput{
					Type:       "sysmon",
					Timestamp:  ts,
					BundleID:   bundleID,
					PID:        targetPID,
					CPUUsage:   &cpu,
					MemUsage:   &memUsage,
					NetInKbps:  netInPtr,
					NetOutKbps: netOutPtr,
				}
				printJSON(out)
			}

		case fpsMsg, ok := <-fpsChan:
			if !ok {
				return
			}
			ts := uint64(time.Now().UnixMilli())

			if humanReadable {
				fmt.Printf("%-20s %-10s %-15s %-15s %-10.2f\n", time.Now().Format("15:04:05.000"), "FPS", "-", "-", fpsMsg.FPS)
			} else {
				fpsVal := fpsMsg.FPS
				out := perfJsonOutput{
					Type:      "fps",
					Timestamp: ts,
					BundleID:  bundleID, // FPS is usually system-wide or foreground, we tag it with bundleID if specified
					PID:       targetPID,
					FPS:       &fpsVal,
				}
				printJSON(out)
			}

		case <-c:
			log.Info("shutting down perf monitoring")
			return
		}
	}
}

func printJSON(v interface{}) {
	b, err := json.Marshal(v)
	if err == nil {
		fmt.Println(string(b))
	}
}
