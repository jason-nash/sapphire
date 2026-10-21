package main

import (
	"fmt"
	"os"
	"runtime"

	"sapphire/guest/internal/guest"
)

func init() {
	// Namespace setup must stay on the process leader through pivot_root/exec.
	runtime.LockOSThread()
}

func main() {
	if len(os.Args) == 2 {
		switch os.Args[1] {
		case "--version":
			fmt.Println("sapphire-guest " + guest.Version)
			return
		case "--help":
			fmt.Println("sapphire-guest: Linux guest PID 1; RPC vsock 1024, Docker proxy 1025, TCP proxy 1026")
			return
		}
	}
	if len(os.Args) == 3 && os.Args[1] == "--check-blank-device" {
		if err := guest.CheckBlankDevice(os.Args[2]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	if len(os.Args) == 3 && os.Args[1] == "__machine-init" {
		if err := guest.MachineInit(os.Args[2]); err != nil {
			guest.ReportInitFailure(err)
			os.Exit(1)
		}
		return
	}
	if len(os.Args) != 1 || os.Getpid() != 1 || os.Geteuid() != 0 {
		fmt.Fprintln(os.Stderr, "sapphire-guest must run as Linux guest PID 1 (root)")
		os.Exit(1)
	}
	guest.RunPID1()
}
