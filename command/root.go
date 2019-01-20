package command

import (
	"github.com/fsnotify/fsevents"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	"os/exec"
	"time"
	"os"
	"reflect"
	"syscall"
	"fmt"
)
type LogFormatter struct {}
var RealFormatter = &log.TextFormatter{}

func (formatter *LogFormatter) Format(entry *log.Entry) ([]byte, error) {
	b, err := RealFormatter.Format(entry)
	if err == nil {
		return append([]byte(fmt.Sprintf("PID=%d ", os.Getpid())), b...), nil
	} else {
		return b, err
	}
}

func init() {
	RootCommand.PersistentFlags().BoolP("verbose", "v", false,
		"Output more information")
	RootCommand.PersistentFlags().BoolP("debug", "d", false,
		"Output debugging information")
	RootCommand.PersistentFlags().Bool("trace", false,
		"Output trace information (more than debug)")
	RootCommand.PersistentFlags().StringP("path", "p", ".",
		"Path to watch")

	log.SetFormatter(&LogFormatter{})
}

func getFlagBool(command *cobra.Command, name string) bool {
	value, err := command.Flags().GetBool(name)
	if err != nil {
		log.Fatal(err)
	}
	return value
}

func getFlagInt(command *cobra.Command, name string) int {
	value, err := command.Flags().GetInt(name)
	if err != nil {
		log.Fatal(err)
	}
	return value
}

func getFlagString(command *cobra.Command, name string) string {
	value, err := command.Flags().GetString(name)
	if err != nil {
		log.Fatal(err)
	}
	return value
}

var RootCommand = &cobra.Command{
	Use:   "restart-on-changes",
	Short: "Restart a process when a directory changes",
	PersistentPreRun: func(command *cobra.Command, args []string) {
		if getFlagBool(command, "trace") {
			log.SetLevel(log.TraceLevel)
		} else if getFlagBool(command, "debug") {
			log.SetLevel(log.DebugLevel)
		} else if getFlagBool(command, "verbose") {
			log.SetLevel(log.InfoLevel)
		} else {
			log.SetLevel(log.WarnLevel)
		}
	},
	Run: func(command *cobra.Command, args []string) {
		paths := []string{getFlagString(command, "path")}
		
		updateChannel := make(chan bool)
		for _, path := range paths {
			go watchPath(updateChannel, path)
		}
		
		for {
			runUntil(args, updateChannel)
			log.Warnf("Restarting child process")
		}
	},
}

func runUntil(command []string, updateChannel chan bool) {
	pgidChannel := make(chan int)
	exitChannel := make(chan ChildExit)
	
	log.Debugf("Starting child process")
	go runCommand(pgidChannel, exitChannel, command)
	pgid := <-pgidChannel
	
	select {
	case result := <-exitChannel:
		// We got an exit before a file system update.
		if result.Err == nil {
			log.Infof("Child process %d exited: success", result.Pid)
			os.Exit(0)
		}
		
		log.Warnf("Child process %d exited: %v", result.Pid, result.Err)
		log.Warnf("Waiting for file change to restart command")
		
		// Wait for an update to restart.
		<-updateChannel
		return
	case <-updateChannel:
		// Got an update without an exit. Continue.
	}
	
	log.Infof("File system changed; restarting")
	
	// The child needs to be restarted
	log.Debugf("Sending SIGTERM to child process group %d", pgid)
	err := syscall.Kill(-pgid, syscall.Signal(syscall.SIGTERM))
	if err != nil {
		log.Warnf("Error killing process [%v]: %v", reflect.TypeOf(err), err)
	}

	select {
	case result := <-exitChannel:
		if result.Err == nil {
			log.Infof("Child process %d exited: success", result.Pid)
			os.Exit(0)
		}
	case <-time.After(500 * time.Millisecond):
		log.Debugf("Sending SIGKILL to child process group %d", pgid)
		err = syscall.Kill(-pgid, syscall.Signal(syscall.SIGKILL))
		if err != nil {
			log.Warnf("Error killing process [%v]: %v", reflect.TypeOf(err), err)
		}
		/// FIXME we should check that it's actually dead.
	}
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
			log.Fatalf("Could not get process group for child %d: %v [%v]",
				pid, err, reflect.TypeOf(err))
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
