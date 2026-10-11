//go:build !darwin && !linux

package broker

import (
	"errors"
	"os"
)

func lockStateFile(*os.File) error {
	return errors.New("durable state locking currently supports Linux and macOS")
}
