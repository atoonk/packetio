//go:build linux && cgo && dpdk && amd64

// Package eal is the only part of the DPDK backend that talks to DPDK.
//
// It is cgo, and it runs on the control path: opening a device, building its
// memory, starting it, and taking it down again. The packet path crosses into C
// three times per batch and no more -- a burst each way and, when a queue has
// gone idle, a poke -- which at about 36 nanoseconds a crossing is under a
// nanosecond a packet at any sensible batch size.
//
// # The environment is process-wide and single-shot
//
// rte_eal_init may be called once in the life of a process, and DPDK cannot be
// re-initialised after rte_eal_cleanup. So this package starts the environment
// lazily on the first Open, keeps it, and hot-plugs devices in and out of it
// afterwards. A program that opens and closes twenty devices initialises the
// EAL once.
//
// It also takes a thread and does not give it back. rte_eal_init sets the
// calling thread's affinity to its main lcore, and a Go worker thread that
// came out of the scheduler's pool would carry that mask into whatever
// goroutine ran on it next -- so the init and every later control call run on a
// goroutine locked to a thread of its own, which nothing else ever uses.
package eal

/*
#cgo CFLAGS: -I/usr/include/dpdk -I/usr/include/x86_64-linux-gnu/dpdk -Wall
#cgo LDFLAGS: -lrte_eal -lrte_log -lrte_ethdev -lrte_mempool -lrte_mbuf -lrte_net -lrte_bus_pci -lrte_bus_vdev -lrte_kvargs -lrte_telemetry -lrte_ring -lrte_hash -lrte_pci
#include <stdlib.h>
#include "shim.h"
*/
import "C"

import (
	"bufio"
	"fmt"
	"os"
	"runtime"
	"strings"
	"sync"
)

const errLen = 512

// call is one piece of control-plane work for the EAL's own goroutine.
type call struct {
	do   func()
	done chan struct{}
}

var (
	mu      sync.Mutex
	started bool
	initErr error
	work    chan call

	logMu   sync.Mutex
	logTail []string
)

// Args are the environment's own arguments, worked out by the caller from what
// the machine has.
type Args struct {
	// Devices are the devices to bring up at init: a PCI address becomes an
	// allow-list entry and anything else a virtual device.
	Devices []string

	// NoHuge runs without hugepages, for a virtual device on a machine that
	// has none reserved. It cannot be combined with the in-memory mode: DPDK
	// refuses the pair with nothing but "Invalid 'command line' arguments"
	// and a usage dump, which is why the two are chosen here rather than by
	// the caller.
	NoHuge bool

	// Memory is how many megabytes to take when NoHuge is set.
	Memory int

	// LogLevel is the EAL's own verbosity, as DPDK spells it.
	LogLevel string

	// Extra is passed through unchanged, for the arguments this package does
	// not name. Each element becomes one argv entry, so nothing here can add
	// an option the caller did not write.
	Extra []string
}

func (a Args) argv() []string {
	out := []string{"packetio", "--no-telemetry"}
	if a.NoHuge {
		mem := a.Memory
		if mem <= 0 {
			mem = 512
		}
		out = append(out, "--no-huge", "-m", fmt.Sprint(mem),
			"--no-shconf", "--file-prefix", fmt.Sprintf("packetio%d", os.Getpid()))
	} else {
		out = append(out, "--in-memory")
	}
	if a.LogLevel != "" {
		out = append(out, "--log-level="+a.LogLevel)
	}
	pci := false
	for _, d := range a.Devices {
		if IsPCI(d) {
			out = append(out, "-a", d)
			pci = true
		} else {
			out = append(out, "--vdev", d)
		}
	}
	if !pci {
		// Without this the EAL scans and binds every PCI device it can, which
		// on a machine whose other NICs are in use is not ours to do.
		out = append(out, "--no-pci")
	}
	// Last, so that a caller can override what is chosen above.
	return append(out, a.Extra...)
}

// IsPCI reports whether a device specification is a PCI address rather than a
// virtual device. A PCI device may carry driver arguments after a comma
// (0000:c1:00.1,dv_flow_en=0), so the classification looks only at the name
// before it -- otherwise the "=" in an argument would make a real PCI device
// look like a vdev, route it to --vdev instead of -a, and fail to attach.
func IsPCI(s string) bool {
	if i := strings.IndexByte(s, ','); i >= 0 {
		s = s[:i]
	}
	if strings.HasPrefix(s, "net_") {
		return false
	}
	// Domain:bus:device.function, or the short bus:device.function form.
	return strings.Count(s, ":") >= 1 && strings.Contains(s, ".")
}

// Init starts the environment if it is not already running, with these
// arguments. A second call with different arguments does not restart anything:
// the environment cannot be restarted, so the first caller's arguments stand
// and this reports what they were if they differ in a way that matters.
func Init(a Args) error {
	mu.Lock()
	defer mu.Unlock()
	if started {
		return initErr
	}
	started = true

	ready := make(chan error, 1)
	work = make(chan call)
	go func() {
		// This goroutine owns its thread for the life of the process. It is
		// never unlocked, so the thread dies with it and is never handed back
		// to the scheduler carrying the EAL's affinity mask.
		runtime.LockOSThread()

		var errbuf [errLen]C.char
		if fd := C.pio_log_open(&errbuf[0], errLen); fd >= 0 {
			go drainLog(int(fd))
		}

		// These strings are deliberately never freed. rte_eal_init keeps the
		// array -- eal_save_args stores the pointers for telemetry to report
		// later -- and getopt permutes it on the way, so freeing them is both
		// a double free and a use-after-free. It aborts the process with
		// "free(): double free detected in tcache 2", which is how this was
		// found. The environment starts once per process, so the leak is a
		// dozen short strings for the life of the program.
		argv := a.argv()
		cargv := make([]*C.char, len(argv))
		for i, s := range argv {
			cargv[i] = C.CString(s)
		}
		rc := C.pio_eal_init(&cargv[0], C.int(len(cargv)), &errbuf[0], errLen)
		if rc != 0 {
			ready <- fmt.Errorf("dpdk: %s%s", C.GoString(&errbuf[0]), logHint())
			return
		}
		ready <- nil

		for c := range work {
			c.do()
			close(c.done)
		}
	}()

	initErr = <-ready
	return initErr
}

// Started reports whether the environment is up.
func Started() bool {
	mu.Lock()
	defer mu.Unlock()
	return started && initErr == nil
}

// onEAL runs f on the environment's own goroutine and waits for it.
//
// Every control-plane call goes through here. They are rare -- open, start,
// stop, close -- and doing them on one thread is what keeps the EAL's idea of
// which lcore it is on true.
func onEAL(f func()) {
	mu.Lock()
	ch := work
	up := started && initErr == nil
	mu.Unlock()
	if !up {
		f() // no environment yet: the caller is about to fail anyway
		return
	}
	c := call{do: f, done: make(chan struct{})}
	ch <- c
	<-c.done
}

// drainLog keeps the last few lines the EAL wrote, so that an error can carry
// what the driver said instead of a bare code.
func drainLog(fd int) {
	f := os.NewFile(uintptr(fd), "dpdk-log")
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		logMu.Lock()
		logTail = append(logTail, line)
		if len(logTail) > 16 {
			logTail = logTail[len(logTail)-16:]
		}
		logMu.Unlock()
	}
}

// logHint is the last thing the EAL said, for appending to an error.
func logHint() string {
	logMu.Lock()
	defer logMu.Unlock()
	if len(logTail) == 0 {
		return ""
	}
	return "; the environment said: " + logTail[len(logTail)-1]
}

// Log returns the last lines the EAL wrote.
func Log() []string {
	logMu.Lock()
	defer logMu.Unlock()
	return append([]string(nil), logTail...)
}

func cerr(buf *C.char, format string, args ...any) error {
	msg := C.GoString(buf)
	if msg == "" {
		msg = "no reason given"
	}
	return fmt.Errorf("dpdk: %s: %s", fmt.Sprintf(format, args...), msg)
}
