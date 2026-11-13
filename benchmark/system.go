package main

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"time"
)

type cpuUsage struct {
	seconds float64
	valid   bool
}

func readCPUUsage() (cpuUsage, error) {
	data, err := os.ReadFile("/proc/self/schedstat")
	if err != nil {
		return cpuUsage{}, fmt.Errorf("read process CPU time: %w", err)
	}
	fields := strings.Fields(string(data))
	if len(fields) == 0 {
		return cpuUsage{}, fmt.Errorf("process CPU metric is malformed")
	}
	nanoseconds, err := strconv.ParseUint(fields[0], 10, 64)
	if err != nil {
		return cpuUsage{}, fmt.Errorf("parse process CPU time: %w", err)
	}
	return cpuUsage{seconds: float64(nanoseconds) / float64(time.Second), valid: true}, nil
}

type memoryUsage struct {
	heapAlloc uint64
	rss       uint64
}

func readMemoryUsage() memoryUsage {
	var stats runtime.MemStats
	runtime.ReadMemStats(&stats)
	return memoryUsage{heapAlloc: stats.HeapAlloc, rss: readRSS()}
}

func readRSS() uint64 {
	data, err := os.ReadFile("/proc/self/statm")
	if err != nil {
		return 0
	}
	fields := strings.Fields(string(data))
	if len(fields) < 2 {
		return 0
	}
	pages, err := strconv.ParseUint(fields[1], 10, 64)
	if err != nil {
		return 0
	}
	return pages * uint64(os.Getpagesize())
}

func collectEnvironment() Environment {
	environment := Environment{
		GoVersion:   runtime.Version(),
		GOOS:        runtime.GOOS,
		GOARCH:      runtime.GOARCH,
		NumCPU:      runtime.NumCPU(),
		GOMAXPROCS:  runtime.GOMAXPROCS(0),
		CPUModel:    readFirstLine("/proc/cpuinfo", "model name"),
		Kernel:      readTrimmed("/proc/sys/kernel/osrelease"),
		Hostname:    hostname(),
		TotalMemory: totalMemory(),
		Commit:      gitCommit(),
		BuildInfo:   buildInfo(),
	}
	return environment
}

func readFirstLine(path, prefix string) string {
	file, err := os.Open(path)
	if err != nil {
		return "unknown"
	}
	defer func() { _ = file.Close() }()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		_, value, found := strings.Cut(line, ":")
		if found {
			return strings.TrimSpace(value)
		}
	}
	return "unknown"
}

func readTrimmed(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return "unknown"
	}
	return strings.TrimSpace(string(data))
}

func hostname() string {
	value, err := os.Hostname()
	if err != nil || value == "" {
		return "unknown"
	}
	return value
}

func totalMemory() uint64 {
	file, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0
	}
	defer func() { _ = file.Close() }()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 2 || fields[0] != "MemTotal:" {
			continue
		}
		kilobytes, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			return 0
		}
		return kilobytes * 1024
	}
	return 0
}

func gitCommit() string {
	command := exec.Command("git", "rev-parse", "HEAD")
	output, err := command.Output()
	if err != nil {
		return "unknown"
	}
	return strings.TrimSpace(string(output))
}

func buildInfo() map[string]string {
	values := make(map[string]string)
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return values
	}
	values["go_version"] = info.GoVersion
	for _, setting := range info.Settings {
		values[setting.Key] = setting.Value
	}
	return values
}
