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
		
		childChannel := make(chan *exec.Cmd)
		exitChannel := make(chan bool)
		for true {
			log.Debugf("Starting child process")
			go runCommand(childChannel, exitChannel, args)
			child := <-childChannel
			
			// Wait for an update to the file system
			<-updateChannel
			log.Infof("Sending SIGTERM to child process %d", child.Process.Pid)
			err := syscall.Kill(child.Process.Pid, syscall.Signal(syscall.SIGTERM))
			if err != nil {
				log.Warnf("Error killing process [%v]: %v", reflect.TypeOf(err), err)
			}
			
			select {
			case <-exitChannel:
				log.Debugf("Child process exited; restarting")
			case <-time.After(500 * time.Millisecond):
				log.Infof("Sending SIGKILL to child process %d", child.Process.Pid)
				err := syscall.Kill(child.Process.Pid, syscall.Signal(syscall.SIGKILL))
				if err != nil {
					log.Warnf("Error killing process [%v]: %v", reflect.TypeOf(err), err)
				}
				/// FIXME we should check that it's actually dead.
			}
		}
	},
}

func runCommand(childChannel chan *exec.Cmd, exitChannel chan bool, args []string) {
	child := exec.Command(args[0], args[1:]...)
	child.Stdin = os.Stdin
	child.Stdout = os.Stdout
	child.Stderr = os.Stderr

	log.Debugf("Starting %v", args)
	err := child.Start()
	if err != nil {
		log.Fatalf("Could not start %v: %v", args, err)
	}
	
	pid := child.Process.Pid
	log.Infof("Child process %d started %v", pid, args)
	childChannel <- child
	
	err = child.Wait()
	if err != nil {
		log.Warnf("Child process %d exited: %v", pid, err)
		log.Warnf("Waiting for file change to restart command")
	} else {
		log.Warnf("Child process %d exited: success", pid)
		os.Exit(0)
	}
	
	exitChannel <- true
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
