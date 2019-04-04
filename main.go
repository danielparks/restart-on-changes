package main

import (
	"flag"
	"fmt"
	"github.com/fsnotify/fsevents"
	log "github.com/sirupsen/logrus"
	"os"
	"os/exec"
	"os/signal"
	"reflect"
	"syscall"
	"time"
)

var RealFormatter = &log.TextFormatter{}

type LogFormatter struct{}

func (formatter *LogFormatter) Format(entry *log.Entry) ([]byte, error) {
	b, err := RealFormatter.Format(entry)
	if err == nil {
		return append([]byte("restart-on-changes "), b...), nil
	} else {
		return b, err
	}
}

var (
	path    string
	verbose bool
	debug   bool
	norun   bool
)

func init() {
	flag.StringVar(&path, "path", ".", "Path to watch")
	flag.StringVar(&path, "p", ".", "Path to watch (shorthand)")
	flag.BoolVar(&verbose, "verbose", false, "Output more information")
	flag.BoolVar(&verbose, "v", false, "Output more information (shorthand)")
	flag.BoolVar(&debug, "debug", false, "Output debugging information")
	flag.BoolVar(&debug, "d", false, "Output debugging information (shorthand)")
	flag.BoolVar(&norun, "norun", false, "Don't run the command, just watch the path")

	log.SetFormatter(&LogFormatter{})
}

func main() {
	flag.Parse()

	if debug {
		log.SetLevel(log.DebugLevel)
	} else if verbose {
		log.SetLevel(log.InfoLevel)
	} else {
		log.SetLevel(log.WarnLevel)
	}

	paths := []string{path}

	updateChan := make(chan bool)
	for _, path := range paths {
		go watchPath(updateChan, path)
	}

	if norun {
		if len(flag.Args()) > 0 {
			log.Warnf("Not running command since -norun was specified")
		}

		// This loop never exits.
		for {
			select {
			case <-updateChan:
			}
		}
	}

	// This loop never exits.
	for {
		runUntil(flag.Args(), updateChan)
		log.Warnf("Restarting child process")
	}
}

func runUntil(command []string, updateChan chan bool) {
	pgidChan := make(chan int)
	exitChan := make(chan ChildExit)

	// Signals do not block, so there must be a buffer:
	signalChan := make(chan os.Signal, 5)
	signal.Notify(signalChan)

	log.Debugf("Starting child process")
	go runCommand(pgidChan, exitChan, command)
	pgid := <-pgidChan

	var result ChildExit

	for {
		select {
		case signal := <-signalChan:
			handleSignal(signal.(syscall.Signal), pgid, exitChan)
		case result = <-exitChan:
			handleChildExit(result, updateChan)
			return // Restart child process
		case <-updateChan:
			handleUpdate(pgid, updateChan, signalChan, exitChan)
			return
		}
	}
}

func handleUpdate(pgid int, updateChan chan bool, signalChan chan os.Signal, exitChan chan ChildExit) {
	// We wait this long for additional events before restarting the child.
	patiencePeriod := 100 * time.Millisecond

	for {
		select {
		case signal := <-signalChan:
			handleSignal(signal.(syscall.Signal), pgid, exitChan)
		case result := <-exitChan:
			handleChildExit(result, updateChan)
			return
		case <-updateChan:
			// Don't care
		case <-time.After(patiencePeriod):
			log.Debugf("patience Chan unblocked")
			// The child needs to be restarted
			err := killAll(pgid, exitChan)
			if err != nil {
				log.Fatalf("Could not kill process group %d: %v", pgid, err)
			}

			return
		}
	}
}

func handleSignal(signal syscall.Signal, pgid int, exitChan chan ChildExit) {
	if isKillSignal(signal) {
		log.Debugf("Received signal %q: killing all processes", signal)
		err := killAll(pgid, exitChan)
		if err != nil {
			log.Errorf("Could not kill process group %d: %v", pgid, err)
		}
		os.Exit(128 + int(signal)) // Exit code includes signal
	}

	// Pass the signal along. FIXME: only to immediate child?
	log.Debugf("Received signal %q: passing it to all processes", signal)
	err := syscall.Kill(-pgid, signal)
	if err != nil {
		log.Warnf("Could not pass received signal %q to children: %v", signal, err)
	}
}

func handleChildExit(result ChildExit, updateChan chan bool) {
	// We got an exit before a file system update.
	if result.Err == nil {
		log.Infof("Child process %d exited: success", result.Pid)
		os.Exit(0)
	}

	signal.Reset()
	log.Warnf("Child process %d exited: %v", result.Pid, result.Err)

	log.Warnf("Waiting for file change to restart command")
	<-updateChan
}

var ErrTimeout = fmt.Errorf("timed out")

func signalProcess(pid int, signal syscall.Signal, exitChan chan ChildExit) error {
	target := fmt.Sprintf("process %d", pid)
	if pid < 0 {
		target = fmt.Sprintf("process group %d", -pid)
	}

	log.Debugf("Sending %v to %s", signal, target)
	err := syscall.Kill(pid, signal)
	if err != nil {
		log.Warnf("Error sending %v to %s: %v (%T)", signal, target, err, err)
		return err
	}

	timeout := 500 * time.Millisecond
	select {
	case <-exitChan:
		return nil // It's no longer running
	case <-time.After(timeout):
		log.Warnf("Timed out sending %v to %s after %v", signal, target, timeout)
		return ErrTimeout
	}
}

func killAll(pgid int, exitChan chan ChildExit) error {
	// signalProcess handles logging
	err := signalProcess(-pgid, syscall.SIGTERM, exitChan)
	if err != nil {
		err = signalProcess(-pgid, syscall.SIGKILL, exitChan)
		if err != nil {
			return err
		}
	}

	return nil
}

func isKillSignal(s os.Signal) bool {
	return int(s.(syscall.Signal)) <= int(syscall.SIGTERM)
}

type ChildExit struct {
	Pid int
	Err error
}

func runCommand(pgidChan chan int, exitChan chan ChildExit, args []string) {
	child := exec.Command(args[0], args[1:]...)
	child.Stdin = os.Stdin
	child.Stdout = os.Stdout
	child.Stderr = os.Stderr

	// Create child in its own process group so we can kill it and its
	// descendants all at once.
	child.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	log.Debugf("Starting %v", args)
	err := child.Start()
	if err != nil {
		log.Fatalf("Could not start %v: %v", args, err)
	}

	pid := child.Process.Pid
	pgid := getPgid(pid)

	// If the process wasn't found, pgid will be 0. child.Wait() will return the
	// exit code, so we'll just skip straight to that.
	if pgid > 0 {
		log.Debugf("Child process %d (PGID %d) started %v", pid, pgid, args)
		pgidChan <- pgid
	}

	err = child.Wait()
	exitChan <- ChildExit{pid, err}
}

// Returns 0 if the process is not found
func getPgid(pid int) int {
	pgid, err := syscall.Getpgid(pid)
	if err != nil {
		switch err.(type) {
		case syscall.Errno:
			if err == syscall.ESRCH {
				// Not found
				return 0
			} else {
				log.Fatalf("Could not get process group for child %d: %v (errno %d)",
					pid, err, err)
			}
		default:
			log.Fatalf("Could not get process group for child %d [%v]: %v",
				pid, reflect.TypeOf(err), err)
		}
	}

	return pgid
}

func watchPath(updateChan chan bool, path string) {
	log.Debugf("Watching path %q", path)

	/// FIXME: aggregate paths per device?
	device, err := fsevents.DeviceForPath(path)
	if err != nil {
		log.Fatalf("Failed to retrieve device for path: %v", err)
	}

	stream := &fsevents.EventStream{
		Paths:   []string{path},
		Latency: 500 * time.Millisecond,
		Device:  device,
		Flags:   fsevents.FileEvents | fsevents.WatchRoot,
	}
	stream.Start()

	for msg := range stream.Events {
		for _, event := range msg {
			logEvent(event)
		}
		updateChan <- true
	}
	log.Fatalf("Ran out of events for path %q", path)
}

var noteDescription = map[fsevents.EventFlags]string{
	fsevents.MustScanSubDirs: "MustScanSubdirs",
	fsevents.UserDropped:     "UserDropped",
	fsevents.KernelDropped:   "KernelDropped",
	fsevents.EventIDsWrapped: "EventIDsWrapped",
	fsevents.HistoryDone:     "HistoryDone",
	fsevents.RootChanged:     "RootChanged",
	fsevents.Mount:           "Mount",
	fsevents.Unmount:         "Unmount",

	fsevents.ItemCreated:       "Created",
	fsevents.ItemRemoved:       "Removed",
	fsevents.ItemInodeMetaMod:  "InodeMetaMod",
	fsevents.ItemRenamed:       "Renamed",
	fsevents.ItemModified:      "Modified",
	fsevents.ItemFinderInfoMod: "FinderInfoMod",
	fsevents.ItemChangeOwner:   "ChangeOwner",
	fsevents.ItemXattrMod:      "XAttrMod",
	fsevents.ItemIsFile:        "IsFile",
	fsevents.ItemIsDir:         "IsDir",
	fsevents.ItemIsSymlink:     "IsSymLink",
}

func logEvent(event fsevents.Event) {
	note := ""
	for bit, description := range noteDescription {
		if event.Flags&bit == bit {
			note += description + " "
		}
	}
	log.Debugf("EventID: %d Path: %s Flags: %s", event.ID, event.Path, note)
}
