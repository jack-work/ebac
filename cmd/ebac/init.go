package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/gluck/ebac/internal/systemd"
)

// ---------- init ----------

func cmdInit(args []string) error {
	fs := flag.NewFlagSet("init", flag.ExitOnError)
	bin := fs.String("bin", "", "path to the ebac binary the units should call")
	_ = fs.Parse(args)
	self := *bin
	if self == "" {
		if p, err := os.Executable(); err == nil {
			self = p
		}
	}
	dir, err := systemd.InstallUnits(self)
	if err != nil {
		return err
	}
	fmt.Printf("installed ebac@.service and ebac@.timer in %s\n", dir)
	fmt.Printf("units will exec: %s poll --critic <name>\n", self)
	return nil
}
