package main

import (
	"flag"
	"fmt"
	"github.com/bmatcuk/doublestar"
	"github.com/fsnotify/fsevents"
	"github.com/sirupsen/logrus"
	"os"
	"os/exec"
	"os/signal"
	"reflect"
	"strings"
	"syscall"
	"time"
)

var RealFormatter = &logrus.TextFormatter{}

type LogFormatter struct{}

func (formatter *LogFormatter) Format(entry *logrus.Entry) ([]byte, error) {
	b, err := RealFormatter.Format(entry)
	if err == nil {
		return append([]byte("restart-on-changes "), b...), nil
	} else {
		return b, err
	}
}

var (
	pathToWatch string
	exclude     string
	verbose     bool
	debug       bool
	norun       bool
)

func init() {
	flag.StringVar(&pathToWatch, "path", ".", "Path to watch")
	flag.StringVar(&pathToWatch, "p", ".", "Path to watch (shorthand)")
	flag.StringVar(&exclude, "exclude", "", "Paths to exclude")
	flag.StringVar(&exclude, "x", "", "Paths to exclude")
	flag.BoolVar(&verbose, "verbose", false, "Output more information")
	flag.BoolVar(&verbose, "v", false, "Output more information (shorthand)")
	flag.BoolVar(&debug, "debug", false, "Output debugging information")
	flag.BoolVar(&debug, "d", false, "Output debugging information (shorthand)")
	flag.BoolVar(&norun, "norun", false, "Don't run the command, just watch the path")

	logrus.SetFormatter(&LogFormatter{})
}

func main() {
	flag.Parse()

	if debug {
		logrus.SetLevel(logrus.DebugLevel)
	} else if verbose {
		logrus.SetLevel(logrus.InfoLevel)
	} else {
		logrus.SetLevel(logrus.WarnLevel)
	}

	if exclude != "" {
		_, err := doublestar.PathMatch(exclude, ".")
		if err != nil {
			logrus.Fatalf("Invalid glob in exclude pattern %q", exclude)
		}
	}

	paths := []string{pathToWatch}

	updateChan := make(chan bool)
	for _, p := range paths {
		go watchPath(updateChan, p)
	}

	if norun {
		if len(flag.Args()) > 0 {
			logrus.Warnf("Not running command since -norun was specified")
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
		logrus.Warnf("Restarting child process")
	}
}

func runUntil(command []string, updateChan chan bool) {
	pgidChan := make(chan int)
	exitChan := make(chan ChildExit)

	// Signals do not block, so there must be a buffer:
	signalChan := make(chan os.Signal, 5)
	signal.Notify(signalChan)

	logrus.Debugf("Starting child process")
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
			handleUpdate(pgid, exitChan)
			return // Restart child process
		}
	}
}

func handleUpdate(pgid int, exitChan chan ChildExit) {
	// The child needs to be restarted
	err := killAll(pgid, exitChan)
	if err != nil {
		logrus.Fatalf("Could not kill process group %d: %v", pgid, err)
	}
}

func handleSignal(signal syscall.Signal, pgid int, exitChan chan ChildExit) {
	if isKillSignal(signal) {
		logrus.Debugf("Received signal %q: killing all processes", signal)
		err := killAll(pgid, exitChan)
		if err != nil {
			logrus.Errorf("Could not kill process group %d: %v", pgid, err)
		}
		os.Exit(128 + int(signal)) // Exit code includes signal
	}

	// Pass the signal along. FIXME: only to immediate child?
	logrus.Debugf("Received signal %q: passing it to all processes", signal)
	err := syscall.Kill(-pgid, signal)
	if err != nil {
		logrus.Warnf("Could not pass received signal %q to children: %v", signal, err)
	}
}

func handleChildExit(result ChildExit, updateChan chan bool) {
	// We got an exit before a file system update.
	if result.Err == nil {
		logrus.Infof("Child process %d exited: success", result.Pid)
		os.Exit(0)
	}

	signal.Reset()
	logrus.Warnf("Child process %d exited: %v", result.Pid, result.Err)

	logrus.Warnf("Waiting for file change to restart command")
	<-updateChan
}

var ErrTimeout = fmt.Errorf("timed out")

func signalProcess(pid int, signal syscall.Signal, exitChan chan ChildExit) error {
	target := fmt.Sprintf("process %d", pid)
	if pid < 0 {
		target = fmt.Sprintf("process group %d", -pid)
	}

	logrus.Debugf("Sending %v to %s", signal, target)
	err := syscall.Kill(pid, signal)
	if err != nil {
		logrus.Warnf("Error sending %v to %s: %v (%T)", signal, target, err, err)
		return err
	}

	timeout := 500 * time.Millisecond
	select {
	case <-exitChan:
		return nil // It's no longer running
	case <-time.After(timeout):
		logrus.Warnf("Timed out sending %v to %s after %v", signal, target, timeout)
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

	logrus.Debugf("Starting %v", args)
	err := child.Start()
	if err != nil {
		logrus.Fatalf("Could not start %v: %v", args, err)
	}

	pid := child.Process.Pid
	pgid := getPgid(pid)

	// If the process wasn't found, pgid will be 0. child.Wait() will return the
	// exit code, so we'll just skip straight to that.
	if pgid > 0 {
		logrus.Debugf("Child process %d (PGID %d) started %v", pid, pgid, args)
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
				logrus.Fatalf("Could not get process group for child %d: %v (errno %d)",
					pid, err, err)
			}
		default:
			logrus.Fatalf("Could not get process group for child %d [%v]: %v",
				pid, reflect.TypeOf(err), err)
		}
	}

	return pgid
}

func devicePrefixForPath(device int32, p string) string {
	stream := &fsevents.EventStream{
		Paths:   []string{p},
		Latency: 0,
		Device:  device,
		Flags:   fsevents.FileEvents,
	}
	stream.Start()

	// Trigger event to get its path.
	err := os.Chtimes(p, time.Now(), time.Now())
	if err != nil {
		logrus.Fatalf("Failed set atime/mtime on %q: %v", p, err)
	}

	prefix := ""
	prefixFound := false

	// If there are multiple events, the shortest path must be ours.
	msg := <-stream.Events
	for _, event := range msg {
		if !prefixFound || len(event.Path) < len(prefix) {
			prefix = event.Path
		}
	}

	logrus.Debugf("got device path prefix %q for path %q", prefix, p)

	return prefix
}

func watchPath(updateChan chan bool, p string) {
	/// FIXME: aggregate paths per device?
	device, err := fsevents.DeviceForPath(p)
	if err != nil {
		logrus.Fatalf("Failed to retrieve device for path %q: %v", p, err)
	}

	prefix := devicePrefixForPath(device, p) + "/"

	stream := &fsevents.EventStream{
		Paths:   []string{p},
		Latency: 500 * time.Millisecond,
		Device:  device,
		Flags:   fsevents.FileEvents | fsevents.IgnoreSelf,
	}
	stream.Start()

	for msg := range stream.Events {
		for _, event := range msg {
			if shouldUpdate(prefix, event) {
				logEvent(prefix, event)
				updateChan <- true
				break // skip the rest of msg
			}
		}
	}
	logrus.Fatalf("Ran out of events for path %q", p)
}

var importantEventBits =
	fsevents.ItemCreated |
	fsevents.ItemModified |
	fsevents.ItemRemoved |
	fsevents.ItemRenamed |
	fsevents.ItemModified |
	fsevents.ItemInodeMetaMod | // chmod and touch
	fsevents.ItemChangeOwner

func shouldUpdate(prefix string, event fsevents.Event) bool {
	relativePath := strings.TrimPrefix(event.Path, prefix)

	if exclude != "" {
		matched, err := doublestar.PathMatch(exclude, relativePath)
		if err != nil {
			logrus.Fatal(err)
		}

		if matched {
			logrus.Debugf("Ignoring events on excluded path %s", relativePath)
			return false
		}
	}

	if event.Flags&fsevents.ItemIsDir > 0 {
		logrus.Debugf("Ignoring events on directory %s", relativePath)
		return false
	}

	if event.Flags&importantEventBits == 0 {
		// Unimportant event
		return false
	}

	// This will be logged outside
	return true
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

func logEvent(prefix string, event fsevents.Event) {
	/// FIXME this silently skips events not listed above (if there are any)
	note := ""
	for bit, description := range noteDescription {
		if event.Flags&bit == bit {
			note += description + " "
		}
	}

	logrus.Debugf("%s events: %s", strings.TrimPrefix(event.Path, prefix), note)
}
