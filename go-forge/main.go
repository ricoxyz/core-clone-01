package main

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"runtime/debug"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func retryDelay(attempt int) time.Duration {
	if attempt > 8 {
		attempt = 8
	}
	ms := 250 << attempt
	if ms > 2000 {
		ms = 2000
	}
	return time.Duration(ms) * time.Millisecond
}

func parseEnvBase(envURL string) (hostPort, hostHdr, basePath string) {
	u := strings.TrimRight(strings.TrimSpace(envURL), "/")
	u = strings.TrimPrefix(u, "http://")
	u = strings.TrimPrefix(u, "https://")
	if slash := strings.IndexByte(u, '/'); slash >= 0 {
		hostHdr = u[:slash]
		basePath = u[slash:]
	} else {
		hostHdr = u
		basePath = ""
	}
	if strings.IndexByte(hostHdr, ':') >= 0 {
		hostPort = hostHdr
	} else {
		hostPort = hostHdr + ":80"
	}
	return hostPort, hostHdr, basePath
}

func envHTTPRequest(hostPort, hostHdr, method, path string, body []byte) (status int, respBody []byte, err error) {
	conn, err := net.DialTimeout("tcp", hostPort, 10*time.Second)
	if err != nil {
		return 0, nil, err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(15 * time.Minute))

	var req bytes.Buffer
	req.Grow(256 + len(body))
	req.WriteString(method)
	req.WriteByte(' ')
	req.WriteString(path)
	req.WriteString(" HTTP/1.1\r\nHost: ")
	req.WriteString(hostHdr)
	req.WriteString("\r\nConnection: close\r\n")
	if len(body) > 0 {
		req.WriteString("Content-Type: application/json\r\nContent-Length: ")
		req.WriteString(strconv.Itoa(len(body)))
		req.WriteString("\r\n\r\n")
		req.Write(body)
	} else {
		req.WriteString("\r\n")
	}

	if _, err := conn.Write(req.Bytes()); err != nil {
		return 0, nil, err
	}
	raw, err := io.ReadAll(conn)
	if err != nil && len(raw) == 0 {
		return 0, nil, err
	}
	sep := bytes.Index(raw, []byte("\r\n\r\n"))
	if sep < 0 {
		return 0, nil, fmt.Errorf("malformed HTTP response from env")
	}
	hdr := raw[:sep]
	respBody = raw[sep+4:]
	nl := bytes.IndexByte(hdr, '\r')
	if nl < 0 {
		nl = len(hdr)
	}
	statusLine := string(hdr[:nl])
	parts := strings.SplitN(statusLine, " ", 3)
	if len(parts) < 2 {
		return 0, respBody, fmt.Errorf("bad status line: %s", statusLine)
	}
	code, perr := strconv.Atoi(parts[1])
	if perr != nil {
		return 0, respBody, perr
	}
	return code, respBody, nil
}

func resolveEnvDialTarget(hostPort string) string {
	if tcpAddr, err := net.ResolveTCPAddr("tcp", hostPort); err == nil && tcpAddr != nil {
		return tcpAddr.String()
	}
	return hostPort
}

func registerWithEnv(envURL string, port int) error {
	hostPort, hostHdr, basePath := parseEnvBase(envURL)
	dialTarget := resolveEnvDialTarget(hostPort)
	payload := []byte(fmt.Sprintf(`{"port":%d}`, port))
	for attempt := 0; ; attempt++ {
		code, body, err := envHTTPRequest(dialTarget, hostHdr, "POST", basePath+"/register", payload)
		if err == nil {
			if code == 200 || code == 409 {
				return nil
			}
			if code == 400 || code == 405 {
				return fmt.Errorf("register answered HTTP %d: %s", code, string(body))
			}
		}
		time.Sleep(retryDelay(attempt))
	}
}

func waitForEnvDone(envURL string) {
	hostPort, hostHdr, basePath := parseEnvBase(envURL)
	dialTarget := resolveEnvDialTarget(hostPort)
	for attempt := 0; ; attempt++ {
		code, body, err := envHTTPRequest(dialTarget, hostHdr, "GET", basePath+"/done", nil)
		if err == nil && code >= 200 && code < 300 {
			if bytes.Contains(body, []byte(`"done":true`)) || bytes.Contains(body, []byte(`"done": true`)) {
				return
			}
		}
		time.Sleep(retryDelay(attempt))
	}
}

func main() {
	// Match 2-CPU sandbox quota and disable periodic GC while guarding OOM with 768 MiB soft limit
	tuneRuntimeForContainer()
	debug.SetGCPercent(-1)
	debug.SetMemoryLimit(768 << 20)
	startTimeCache()
	prewarmPools()

	listenAddr := os.Getenv("FORGE_LISTEN_ADDR")
	if listenAddr == "" {
		listenAddr = "0.0.0.0:0"
	}

	store := NewForgeStore()
	srv := NewServer(store)
	port, err := srv.Listen(listenAddr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[go-forge] failed to listen: %v\n", err)
		os.Exit(1)
	}

	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		srv.Close()
		os.Exit(0)
	}()

	envURL := strings.TrimRight(os.Getenv("MAKEFUN_ENV_URL"), "/")
	if envURL == "" || os.Getenv("FORGE_STANDALONE") == "1" {
		// Standalone / local benchmark mode
		fmt.Printf("READY %d\n", port)
		srv.Serve()
		return
	}

	// Single-process trial pod mode (PID 1 via os.execv from main.py)
	go srv.Serve()

	if err := registerWithEnv(envURL, port); err != nil {
		fmt.Fprintf(os.Stderr, "[entry] %v\n", err)
		srv.Close()
		os.Exit(1)
	}

	waitForEnvDone(envURL)
	srv.Close()
	os.Exit(0)
}
