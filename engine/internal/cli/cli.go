package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"sapphire/engine/internal/client"
	"sapphire/engine/internal/config"
	"sapphire/engine/internal/daemon"
	"sapphire/engine/internal/protocol"
)

func Run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	c, err := config.Default()
	if err != nil {
		return fail(stdout, stderr, commandIsCall(args), err)
	}
	flags := flag.NewFlagSet("sapphire-engine", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.StringVar(&c.Home, "home", c.Home, "private runtime directory (or SAPPHIRE_HOME)")
	flags.StringVar(&c.Driver, "vmm", c.Driver, "signed native driver (or SAPPHIRE_VMM)")
	flags.IntVar(&c.CPUs, "cpus", c.CPUs, "VM CPU count")
	flags.IntVar(&c.MemoryMiB, "memory-mib", c.MemoryMiB, "VM memory in MiB")
	flags.Int64Var(&c.DataGiB, "data-gib", c.DataGiB, "data disk size in GiB; existing disks must match")
	flags.StringVar(&c.Subnet, "subnet", c.Subnet, "private IPv4 /24 subnet")
	flags.StringVar(&c.MAC, "mac", c.MAC, "guest Ethernet MAC (currently fixed at 5a:94:ef:e4:0c:ee)")
	flags.DurationVar(&c.StartupTimeout, "startup-timeout", c.StartupTimeout, "bounded guest readiness wait")
	flags.DurationVar(&c.StopTimeout, "stop-timeout", c.StopTimeout, "guest shutdown grace before signaling our VMM child")
	jsonOutput := flags.Bool("json", false, "print host status as JSON")
	readyFD := flags.Int("ready-fd", -1, "internal startup notification descriptor")
	flags.Usage = func() {
		fmt.Fprintln(stderr, "Usage: sapphire-engine [flags] start|status|stop|call '<JSON>'|serve [flags]")
		flags.PrintDefaults()
	}
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return fail(stdout, stderr, commandIsCall(args), err)
	}
	rest := flags.Args()
	if len(rest) == 0 {
		flags.Usage()
		return 2
	}
	command := rest[0]
	if err := flags.Parse(rest[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return fail(stdout, stderr, command == "call", err)
	}
	rest = flags.Args()
	if err := c.Validate(); err != nil {
		return fail(stdout, stderr, command == "call", err)
	}
	if command != "serve" && *readyFD != -1 {
		return fail(stdout, stderr, command == "call", errors.New("ready-fd is internal to serve"))
	}
	if command != "call" && len(rest) != 0 {
		return fail(stdout, stderr, false, fmt.Errorf("unexpected arguments: %v", rest))
	}
	if command == "serve" {
		if *readyFD != -1 && *readyFD != 3 {
			return fail(stdout, stderr, false, errors.New("ready-fd must be 3"))
		}
		if runtime.GOOS != "darwin" {
			return fail(stdout, stderr, false, errors.New("Sapphire's native Hypervisor.framework driver requires macOS"))
		}
		syscall.Umask(0077)
		signals, cancel := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
		defer cancel()
		ready := func(err error) {
			if *readyFD == -1 {
				return
			}
			file := os.NewFile(uintptr(*readyFD), "startup-notification")
			defer file.Close()
			response := protocol.Success(nil)
			if err != nil {
				response = protocol.Failure(err)
			}
			json.NewEncoder(file).Encode(response)
		}
		if err := daemon.Run(signals, c, ready); err != nil {
			return fail(stdout, stderr, false, err)
		}
		return 0
	}
	control := client.New(c)
	defer control.Close()
	switch command {
	case "call":
		if len(rest) != 1 {
			return fail(stdout, stderr, true, errors.New("call requires exactly one JSON request"))
		}
		request, err := daemon.DecodeRequest(rest[0])
		if err != nil {
			return fail(stdout, stderr, true, err)
		}
		requestCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
		defer cancel()
		response := control.Call(requestCtx, request)
		if err := json.NewEncoder(stdout).Encode(response); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		if !response.OK {
			return 1
		}
		return 0
	case "status":
		requestCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		status, err := control.Status(requestCtx)
		if err != nil {
			return fail(stdout, stderr, false, err)
		}
		return printStatus(stdout, stderr, status, *jsonOutput)
	case "start":
		startCtx, cancel := context.WithTimeout(ctx, c.StartupTimeout+5*time.Second)
		defer cancel()
		executable, err := os.Executable()
		if err != nil {
			return fail(stdout, stderr, false, err)
		}
		status, err := control.Start(startCtx, executable)
		if err != nil {
			return fail(stdout, stderr, false, err)
		}
		var cpuExplicit, memoryExplicit bool
		flags.Visit(func(value *flag.Flag) {
			cpuExplicit = cpuExplicit || value.Name == "cpus"
			memoryExplicit = memoryExplicit || value.Name == "memory-mib"
		})
		if (cpuExplicit && status.CPUs != c.CPUs) || (memoryExplicit && status.MemoryMiB != c.MemoryMiB) {
			return fail(stdout, stderr, false, fmt.Errorf(
				"engine is running with %d CPUs and %d MiB; stop it before changing explicit resource settings",
				status.CPUs, status.MemoryMiB))
		}
		return printStatus(stdout, stderr, status, *jsonOutput)
	case "stop":
		stopCtx, cancel := context.WithTimeout(ctx, c.StopTimeout+12*time.Second)
		defer cancel()
		if err := control.Stop(stopCtx); err != nil {
			return fail(stdout, stderr, false, err)
		}
		if *jsonOutput {
			return printStatus(stdout, stderr, daemon.Stopped(c), true)
		}
		fmt.Fprintln(stdout, "Sapphire engine stopped")
		return 0
	default:
		return fail(stdout, stderr, false, fmt.Errorf("unknown command %q", command))
	}
}

func printStatus(stdout, stderr io.Writer, status daemon.HostStatus, jsonOutput bool) int {
	if jsonOutput {
		if err := json.NewEncoder(stdout).Encode(status); err != nil {
			return fail(stdout, stderr, false, err)
		}
	} else if status.Running {
		fmt.Fprintf(stdout, "Sapphire engine running (pid %d, %d CPUs, %d MiB, %s)\n",
			status.PID, status.CPUs, status.MemoryMiB, status.Backend)
	} else {
		fmt.Fprintf(stdout, "Sapphire engine %s\n", status.State)
	}
	return 0
}

func commandIsCall(args []string) bool {
	for _, arg := range args {
		if arg == "call" {
			return true
		}
	}
	return false
}

func fail(stdout, stderr io.Writer, envelope bool, err error) int {
	if envelope {
		json.NewEncoder(stdout).Encode(protocol.Failure(err))
	} else {
		fmt.Fprintln(stderr, "sapphire-engine:", err)
	}
	return 1
}
