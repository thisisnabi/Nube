package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"os/exec"
	"time"
)

// The sandbox server runs inside a sandbox container of an agent run's pod
// ("nube sandbox", the binary is mounted in). It executes the shell commands
// the model asks for, so they never run in the runtime itself.

type sandboxResult struct {
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	ExitCode int    `json:"exitCode"`
	TimedOut bool   `json:"timedOut,omitempty"`
}

const maxSandboxOutput = 64 << 10

// limitedBuffer keeps the first n bytes and counts the rest.
type limitedBuffer struct {
	buf     bytes.Buffer
	n       int
	dropped int
}

func (l *limitedBuffer) Write(p []byte) (int, error) {
	if room := l.n - l.buf.Len(); room > 0 {
		if len(p) > room {
			l.buf.Write(p[:room])
			l.dropped += len(p) - room
		} else {
			l.buf.Write(p)
		}
	} else {
		l.dropped += len(p)
	}
	return len(p), nil
}

func (l *limitedBuffer) String() string {
	s := l.buf.String()
	if l.dropped > 0 {
		s += "\n... (" + itoa(l.dropped) + " more bytes)"
	}
	return s
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

// Sandbox serves POST /exec {"command": "...", "timeoutSeconds": 60}.
func Sandbox(listen, workdir string) error {
	if workdir != "" {
		if err := os.MkdirAll(workdir, 0o755); err != nil {
			return err
		}
	}
	shell := "/bin/sh"
	if _, err := os.Stat(shell); err != nil {
		return errors.New("the sandbox image needs /bin/sh")
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /exec", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Command        string `json:"command"`
			TimeoutSeconds int    `json:"timeoutSeconds"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Command == "" {
			http.Error(w, `body must be {"command": "..."}`, 400)
			return
		}
		if req.TimeoutSeconds <= 0 {
			req.TimeoutSeconds = 60
		}
		ctx, cancel := context.WithTimeout(r.Context(), time.Duration(req.TimeoutSeconds)*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, shell, "-c", req.Command)
		cmd.Dir = workdir
		stdout, stderr := &limitedBuffer{n: maxSandboxOutput}, &limitedBuffer{n: maxSandboxOutput}
		cmd.Stdout, cmd.Stderr = stdout, stderr
		err := cmd.Run()
		res := sandboxResult{}
		var ee *exec.ExitError
		switch {
		case ctx.Err() == context.DeadlineExceeded:
			res.TimedOut, res.ExitCode = true, -1
		case errors.As(err, &ee):
			res.ExitCode = ee.ExitCode()
		case err != nil:
			res.ExitCode = -1
			stderr.Write([]byte(err.Error()))
		}
		res.Stdout, res.Stderr = stdout.String(), stderr.String()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(res)
	})
	log.Printf("sandbox: listening on %s (workdir %s)", listen, workdir)
	return http.ListenAndServe(listen, mux)
}
