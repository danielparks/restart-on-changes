package main

import (
	"fmt"
	"github.com/DavidGamba/go-getoptions"
	"github.com/bmatcuk/doublestar"
	"github.com/fsnotify/fsevents"
	"github.com/sirupsen/logrus"
	"os"
	"os/exec"
	"os/signal"
	"path"
	"reflect"
	"strings"
	"syscall"
	"time"
)

func main() {
	logrus.SetFormatter(&LogFormatter{})

	command, paths, excludes := processOptions()

	updateChan := make(chan bool)
	for _, p := range paths {
		go watchPath(updateChan, p, excludes)
	}

	if len(command) == 0 {
		// Don't run, just watch the file system
		if logrus.GetLevel() < logrus.InfoLevel { // if not warning or above
			logrus.SetLevel(logrus.InfoLevel) // otherwise there will be no output
		}

		// This loop never exits.
		for {
			<-updateChan
		}
	}

	// Run the command. This loop never exits.
	for {
		runUntil(command, updateChan)
		logrus.Warnf("Restarting child process")
	}
}

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

func processOptions() (command, paths, excludes []string) {
	opt := getoptions.New()
	opt.Bool("help", false, opt.Alias("h", "?"))
	opt.Bool("debug", false, opt.Alias("d"))
	opt.Bool("verbose", false, opt.Alias("v"))

	// See comment below about how this is fucked up.
	shellPtr := opt.NBool("shell", true, opt.Alias("s"))

	pathsPtr := opt.StringSlice("path", 1, 1, opt.Alias("p"),
		opt.Description("Paths to watch. Defaults to the current directory."))

	excludesPtr := opt.StringSlice("exclude", 1, 1, opt.Alias("x"), opt.ArgName("glob"),
		opt.Description("Paths to exclude. Use **/ to match any descendents."))

	opt.SetMode("bundling") // -opt == -o -p -t
	opt.SetRequireOrder()   // stop processing after the first argument is found

	command, err := opt.Parse(os.Args[1:])
	if err != nil {
		logrus.Errorf("Error parsing arguments: %v", err)
		fmt.Fprintf(os.Stderr, opt.Help())
		os.Exit(1)
	}

	if opt.Called("help") {
		fmt.Print(opt.Help())
		os.Exit(0)
	}

	if opt.Called("debug") {
		logrus.SetLevel(logrus.DebugLevel)
	} else if opt.Called("verbose") {
		logrus.SetLevel(logrus.InfoLevel)
	} else {
		logrus.SetLevel(logrus.WarnLevel)
	}

	// Fuck shit PC Load Letter. getopt interprets the default as the value for
	// `--no-shell` in *addition* to being the default. `--no-shell` is always
	// the default. The default is included in the `--help` output.
	if opt.Called("shell") {
		*shellPtr = !*shellPtr
	} else {
		*shellPtr = true
	}

	if *shellPtr {
		command = shellConvert(command)
	}

	// Default to watching the current directory.
	paths = *pathsPtr
	if len(paths) == 0 {
		paths = []string{"."}
	}

	excludes = processExcludeOption(*excludesPtr)
	logrus.Debugf("Excluding: %v", excludes)

	return
}

func processExcludeOption(patterns []string) (paths []string) {
	for _, glob := range patterns {
		paths = append(paths, glob)
		_, err := doublestar.PathMatch(glob, ".")
		if err != nil {
			logrus.Fatalf("Invalid glob in exclude pattern %q", glob)
		}

		// When glob matches a directory we want to exclude its descendants too.
		glob = path.Join(glob, "**")
		paths = append(paths, glob)
		_, err = doublestar.PathMatch(glob, ".")
		if err != nil {
			logrus.Fatalf("Invalid glob in exclude pattern %q", glob)
		}
	}

	return paths
}

func shellConvert(command []string) (shellCommand []string) {
	return []string{"bash", "-c", strings.Join(command, " ")}
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

func watchPath(updateChan chan bool, p string, excludes []string) {
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
			if shouldUpdate(prefix, event, excludes) {
				logEvent(prefix, event)
				updateChan <- true
				break // skip the rest of msg
			}
		}
	}
	logrus.Fatalf("Ran out of events for path %q", p)
}

var importantEventBits = fsevents.ItemCreated |
	fsevents.ItemModified |
	fsevents.ItemRemoved |
	fsevents.ItemRenamed |
	fsevents.ItemModified |
	fsevents.ItemInodeMetaMod | // chmod and touch
	fsevents.ItemChangeOwner

func shouldUpdate(prefix string, event fsevents.Event, excludes []string) bool {
	relativePath := strings.TrimPrefix(event.Path, prefix)

	for _, glob := range excludes {
		matched, err := doublestar.PathMatch(glob, relativePath)
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

	logrus.Infof("%s events: %s", strings.TrimPrefix(event.Path, prefix), note)
}
