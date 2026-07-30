package supervisor

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"

	"github.com/demirtechcom/iold/internal/redact"
)

const (
	logProxyArg     = "__iold_internal_log_proxy"
	logProxyPathEnv = "IOLD_INTERNAL_LOG_PROXY_PATH"
	logProxyFD      = 3
)

// A detached runtime cannot safely write through a goroutine owned by the
// short-lived CLI. Re-exec the same binary as a tiny redacting log proxy; the
// runtime inherits only the pipe writer and the proxy persists until EOF.
func init() {
	if len(os.Args) > 1 && os.Args[1] == logProxyArg {
		os.Exit(runLogProxy())
	}
}

func startLogProxy(logPath string) (*os.File, error) {
	readPipe, writePipe, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("create log pipe: %w", err)
	}
	executable, err := os.Executable()
	if err != nil {
		readPipe.Close()
		writePipe.Close()
		return nil, fmt.Errorf("resolve log proxy executable: %w", err)
	}
	helper := exec.Command(executable, logProxyArg)
	helper.Env = append(os.Environ(), logProxyPathEnv+"="+logPath)
	helper.ExtraFiles = []*os.File{readPipe}
	helper.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := helper.Start(); err != nil {
		readPipe.Close()
		writePipe.Close()
		return nil, fmt.Errorf("start log proxy: %w", err)
	}
	readPipe.Close()
	go func() { _ = helper.Wait() }()
	return writePipe, nil
}

func runLogProxy() int {
	path := os.Getenv(logProxyPathEnv)
	if path == "" {
		return 2
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return 1
	}
	if err := os.Chmod(filepath.Dir(path), 0o700); err != nil {
		return 1
	}
	logFile, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return 1
	}
	defer logFile.Close()
	if err := logFile.Chmod(0o600); err != nil {
		return 1
	}
	pipe := os.NewFile(logProxyFD, "iold-runtime-log")
	if pipe == nil {
		return 1
	}
	defer pipe.Close()
	if err := redact.Copy(logFile, pipe); err != nil {
		return 1
	}
	return 0
}
