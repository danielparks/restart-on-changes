package main

import (
	"fmt"
	"github.com/danielparks/restart-on-changes/command"
	"os"
)

func main() {
	err := command.RootCommand.Execute()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
