//go:build unix

package processtree

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

const processTreeHelperEnv = "PLASMID_PROCESS_TREE_HELPER"

func TestConfigureAttachAndTerminateProcessTree(t *testing.T) {
	childPIDPath := filepath.Join(t.TempDir(), "child.pid")
	command := exec.Command(os.Args[0], "-test.run=^TestProcessTreeHelper$")
	command.Env = append(os.Environ(), processTreeHelperEnv+"=parent", "PLASMID_PROCESS_TREE_CHILD_PID="+childPIDPath)
	if err := Configure(command); err != nil {
		t.Fatal(err)
	}
	if command.SysProcAttr == nil || !command.SysProcAttr.Setpgid {
		t.Fatal("Configure() did not isolate the helper process group")
	}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	tree, err := Attach(command.Process)
	if err != nil {
		t.Fatal(err)
	}
	childPID := waitForHelperChild(t, childPIDPath)
	if err := tree.Terminate(); err != nil {
		t.Fatal(err)
	}
	if err := waitForCommand(command, 5*time.Second); err == nil {
		t.Fatal("process tree helper exited without termination")
	}
	if err := tree.Terminate(); err != nil {
		t.Fatalf("second Terminate() error = %v", err)
	}
	waitForProcessGone(t, childPID)
}

func TestProcessTreeHelper(t *testing.T) {
	switch os.Getenv(processTreeHelperEnv) {
	case "":
		return
	case "child":
		select {}
	case "parent":
		child := exec.Command(os.Args[0], "-test.run=^TestProcessTreeHelper$")
		child.Env = append(os.Environ(), processTreeHelperEnv+"=child")
		if err := child.Start(); err != nil {
			os.Exit(2)
		}
		if err := os.WriteFile(os.Getenv("PLASMID_PROCESS_TREE_CHILD_PID"), []byte(strconv.Itoa(child.Process.Pid)), 0o600); err != nil {
			os.Exit(3)
		}
		select {}
	default:
		os.Exit(4)
	}
}

func waitForHelperChild(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err == nil {
			pid, parseErr := strconv.Atoi(strings.TrimSpace(string(data)))
			if parseErr != nil || pid <= 0 {
				t.Fatalf("helper child PID = %q, error = %v", data, parseErr)
			}
			return pid
		}
		if !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("process tree helper did not start a child")
	return 0
}

func waitForCommand(command *exec.Cmd, timeout time.Duration) error {
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	select {
	case err := <-done:
		return err
	case <-time.After(timeout):
		return errors.New("timed out waiting for helper process")
	}
}

func waitForProcessGone(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		err := syscall.Kill(pid, 0)
		if errors.Is(err, syscall.ESRCH) {
			return
		}
		if err != nil {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("process %d survived process tree termination", pid)
}
