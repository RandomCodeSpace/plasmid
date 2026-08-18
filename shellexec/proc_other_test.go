//go:build !unix

package shellexec

import (
	"errors"
	"os"
	"os/exec"
	"testing"
	"time"
)

func TestOtherProcessHelper(t *testing.T) {
	if os.Getenv("GO_WANT_OTHER_PROCESS_HELPER") != "1" {
		return
	}
	time.Sleep(30 * time.Second)
}

func TestForceSignalTerminatesProcess(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=^TestOtherProcessHelper$")
	cmd.Env = append(os.Environ(), "GO_WANT_OTHER_PROCESS_HELPER=1")
	configureProcess(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if err := signalProcessGroup(cmd.Process, true); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err == nil || !errors.Is(err, os.ErrProcessDone) && !isCommandExit(err) {
		t.Fatalf("Wait() error = %v, want terminated process", err)
	}
}
