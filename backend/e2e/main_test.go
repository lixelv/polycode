package e2e

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// allowedOrigin is the one cross-origin host the server is started with
// (ALLOWED_ORIGINS), as a dev server on another port would be.
const allowedOrigin = "http://dev.example:5173"

var (
	// base is the running server, e.g. http://127.0.0.1:53211.
	base string
	// skipReason is set when the services are not configured.
	skipReason string
	logs       lockedBuffer
)

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestMain(m *testing.M) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	endpoint := strings.TrimPrefix(os.Getenv("S3_TEST_ENDPOINT"), "http://")
	switch {
	case dsn == "":
		skipReason = "TEST_DATABASE_URL is not set"
	case endpoint == "":
		skipReason = "S3_TEST_ENDPOINT is not set"
	}
	if skipReason != "" {
		os.Exit(m.Run())
	}

	dir, err := os.MkdirTemp("", "polycode-e2e-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	code := run(m, dir, dsn, endpoint)
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

func run(m *testing.M, dir, dsn, endpoint string) int {
	bin := filepath.Join(dir, "server")
	build := exec.Command("go", "build", "-race", "-o", bin, "../cmd/server")
	build.Stdout, build.Stderr = os.Stderr, os.Stderr
	if err := build.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "build the server:", err)
		return 1
	}

	addr, err := freeAddr()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	base = "http://" + addr
	region := os.Getenv("S3_TEST_REGION")
	if region == "" {
		region = "us-east-1"
	}
	ssl := "false"
	if os.Getenv("S3_TEST_SSL") == "true" {
		ssl = "true"
	}
	srv := exec.Command(bin)
	srv.Env = append(os.Environ(),
		"DATABASE_URL="+dsn,
		"LISTEN_ADDR="+addr,
		"ALLOWED_ORIGINS="+strings.TrimPrefix(allowedOrigin, "http://"),
		"PROXY_ALLOW_CIDRS=",
		"S3_ENDPOINT="+endpoint,
		"S3_PUBLIC_ENDPOINT="+endpoint,
		"S3_BUCKET="+os.Getenv("S3_TEST_BUCKET"),
		"S3_ACCESS_KEY="+os.Getenv("S3_TEST_ACCESS_KEY"),
		"S3_SECRET_KEY="+os.Getenv("S3_TEST_SECRET_KEY"),
		"S3_REGION="+region,
		"S3_USE_SSL="+ssl,
	)
	if os.Getenv("E2E_MAKE_BUCKET") == "1" {
		// A fresh local S3 has no bucket yet: the server's dev mode makes it.
		srv.Env = append(srv.Env, "S3_DEV_BUCKET_ORIGIN=")
	}
	srv.Stdout, srv.Stderr = &logs, &logs
	if err := srv.Start(); err != nil {
		fmt.Fprintln(os.Stderr, "start the server:", err)
		return 1
	}
	exited := make(chan error, 1)
	go func() { exited <- srv.Wait() }()
	stop := func() {
		_ = srv.Process.Signal(syscall.SIGTERM)
		select {
		case <-exited:
		case <-time.After(15 * time.Second):
			_ = srv.Process.Kill()
			<-exited
		}
	}

	if err := waitHealthy(exited); err != nil {
		stop()
		fmt.Fprintf(os.Stderr, "the server did not come up: %v\n--- server log ---\n%s", err, logs.String())
		return 1
	}
	code := m.Run()
	stop()
	if code != 0 || testing.Verbose() {
		fmt.Fprintf(os.Stderr, "--- server log ---\n%s", logs.String())
	}
	if strings.Contains(logs.String(), "WARNING: DATA RACE") {
		fmt.Fprintln(os.Stderr, "the server reported a data race")
		code = 1
	}
	return code
}

func freeAddr() (string, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	defer l.Close()
	return l.Addr().String(), nil
}

func waitHealthy(exited <-chan error) error {
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-exited:
			return fmt.Errorf("exited: %v", err)
		default:
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, base+"/api/health", nil)
		resp, err := http.DefaultClient.Do(req)
		cancel()
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("no healthy answer from %s", base)
}

// need skips a test when the services are not configured.
func need(t *testing.T) {
	t.Helper()
	if skipReason != "" {
		t.Skip(skipReason)
	}
}
