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

	updateChannel := make(chan bool)
	for _, path := range paths {
		go watchPath(updateChannel, path)
	}

	if norun {
		if len(flag.Args()) > 0 {
			log.Warnf("Not running command since -norun was specified")
		}

		// This loop never exits.
		for {
			select {
			case <-updateChannel:
			}
		}
	}

	// This loop never exits.
	for {
		runUntil(flag.Args(), updateChannel)
		log.Warnf("Restarting child process")
	}
}

func runUntil(command []string, updateChannel chan bool) {
	pgidChannel := make(chan int)
	exitChannel := make(chan ChildExit)

	// Signals do not block, so there must be a buffer:
	signalChannel := make(chan os.Signal, 5)
	signal.Notify(signalChannel)

	log.Debugf("Starting child process")
	go runCommand(pgidChannel, exitChannel, command)
	pgid := <-pgidChannel

	var s os.Signal
	var result ChildExit

	for {
		select {
		case s = <-signalChannel:
			if isKillSignal(s) {
				log.Debugf("Received signal %q: killing all processes", s)
				err := killAll(pgid, exitChannel)
				if err != nil {
					log.Errorf("Could not kill process group %d: %v", pgid, err)
				}
				os.Exit(128 + int(s.(syscall.Signal))) // Exit code includes signal
			}

			// Pass the signal along. FIXME: only to immediate child?
			log.Debugf("Received signal %q: passing it to all processes", s)
			err := syscall.Kill(-pgid, s.(syscall.Signal))
			if err != nil {
				log.Warnf("Could not pass received signal %q to children", s)
			}

			// Loop
		case result = <-exitChannel:
			// We got an exit before a file system update.
			if result.Err == nil {
				log.Infof("Child process %d exited: success", result.Pid)
				os.Exit(0)
			}

			signal.Reset()
			log.Warnf("Child process %d exited: %v", result.Pid, result.Err)

			log.Warnf("Waiting for file change to restart command")
			<-updateChannel

			return
		case <-updateChannel:
			// Got an update without an exit.
			log.Infof("File system changed; restarting")

			// The child needs to be restarted
			err := killAll(pgid, exitChannel)
			if err != nil {
				log.Fatalf("Could not kill process group %d: %v", result.Pid, err)
			}

			return
		}
	}
}

var ErrTimeout = fmt.Errorf("timed out")

func signalProcess(pid int, signal syscall.Signal, exitChannel chan ChildExit) error {
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
	case <-exitChannel:
		return nil // It's no longer running
	case <-time.After(timeout):
		log.Warnf("Timed out sending %v to %s after %v", signal, target, timeout)
		return ErrTimeout
	}
}

func killAll(pgid int, exitChannel chan ChildExit) error {
	// signalProcess handles logging
	err := signalProcess(-pgid, syscall.SIGTERM, exitChannel)
	if err != nil {
		err = signalProcess(-pgid, syscall.SIGKILL, exitChannel)
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

func runCommand(pgidChannel chan int, exitChannel chan ChildExit, args []string) {
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
		pgidChannel <- pgid
	}

	err = child.Wait()
	exitChannel <- ChildExit{pid, err}
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

func watchPath(updateChannel chan bool, path string) {
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
		updateChannel <- true
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
