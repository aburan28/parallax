/*
Copyright 2026 The Parallax Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package pluginhost

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/aburan28/parallax/pkg/plugin"
	pluginv1 "github.com/aburan28/parallax/proto/plugin/v1"
)

// launch starts a plugin subprocess, reads and parses its stdout handshake, dials the
// unix socket, negotiates the ABI via Describe, and validates config via Configure.
// On any failure the child is killed and reaped before returning a wrapped error.
//
// The process is started with plain exec.Command (not CommandContext): a plugin must
// outlive the short-lived reconcile context that triggered its load — it is stopped
// only via Unload/Close/reload.
func (h *host) launch(ctx context.Context, spec launchSpec) (*entry, error) {
	if _, err := os.Stat(spec.binPath); err != nil {
		return nil, fmt.Errorf("plugin binary %q not found in %s: %w", spec.binName, h.opts.PluginDir, err)
	}

	// TODO(m2): cosign-verify spec.image here (unless h.opts.SkipVerify), resolving the
	// signed digest and enforcing VerifySpec.RequireDigestPin before exec (§5.3).
	if !h.opts.SkipVerify {
		h.log.V(1).Info("cosign verification not yet implemented (M2); proceeding", "plugin", spec.name, "image", spec.image)
	}

	cmd := exec.Command(spec.binPath)
	cmd.Env = append(os.Environ(),
		plugin.MagicEnv+"="+plugin.MagicValue,
		plugin.SocketDirEnv+"="+h.opts.SocketDir,
	)
	cmd.Stderr = os.Stderr // inherit stderr: plugin diagnostics flow to the operator log

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("plugin %q: stdout pipe: %w", spec.name, err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("plugin %q: start %s: %w", spec.name, spec.binPath, err)
	}

	line, err := readHandshakeLine(stdout, plugin.HandshakeTimeout)
	if err != nil {
		killAndReap(cmd)
		return nil, fmt.Errorf("plugin %q: handshake: %w", spec.name, err)
	}
	hs, err := plugin.ParseHandshake(line)
	if err != nil {
		killAndReap(cmd)
		return nil, fmt.Errorf("plugin %q: %w", spec.name, err)
	}

	// Drain any remaining stdout so a chatty child never blocks on a full pipe.
	// cmd.Wait (in terminate/killAndReap) completes once this copy reaches EOF.
	// TODO(m1): a background reaper to detect an unexpected child exit (crash) and
	// mark the entry degraded without waiting for the next health tick.
	go func() { _, _ = io.Copy(io.Discard, stdout) }()

	conn, err := plugin.Dial(ctx, hs)
	if err != nil {
		killAndReap(cmd)
		return nil, fmt.Errorf("plugin %q: %w", spec.name, err)
	}

	rpcCtx, cancel := context.WithTimeout(ctx, plugin.HandshakeTimeout)
	defer cancel()
	lc := pluginv1.NewLifecycleClient(conn)

	desc, err := lc.Describe(rpcCtx, &pluginv1.DescribeRequest{HostAbiVersions: h.opts.HostABIVersions})
	if err != nil {
		_ = conn.Close()
		killAndReap(cmd)
		return nil, fmt.Errorf("plugin %q: Describe: %w", spec.name, err)
	}
	abi := desc.GetSelectedAbiVersion()
	if abi == "" {
		_ = conn.Close()
		killAndReap(cmd)
		return nil, fmt.Errorf("plugin %q: no ABI overlap (plugin offers %v, host offers %v)",
			spec.name, desc.GetAbiVersions(), h.opts.HostABIVersions)
	}

	confResp, err := lc.Configure(rpcCtx, &pluginv1.ConfigureRequest{
		ConfigJson: spec.configJSON,
		AbiVersion: abi,
	})
	if err != nil {
		_ = conn.Close()
		killAndReap(cmd)
		return nil, fmt.Errorf("plugin %q: Configure: %w", spec.name, err)
	}
	if !confResp.GetAccepted() {
		_ = conn.Close()
		killAndReap(cmd)
		return nil, fmt.Errorf("plugin %q: config rejected: %s", spec.name, confResp.GetReason())
	}

	return &entry{
		name:       spec.name,
		kind:       spec.kind,
		binName:    spec.binName,
		binPath:    spec.binPath,
		image:      spec.image,
		configJSON: spec.configJSON,
		cmd:        cmd,
		conn:       conn,
		handshake:  hs,
		digest:     h.resolveDigest(spec),
		abi:        abi,
		ready:      true,
	}, nil
}

// readHandshakeLine reads the child's stdout line-by-line until the single handshake
// line (or timeout / early exit). Any non-handshake noise is ignored.
func readHandshakeLine(r io.Reader, timeout time.Duration) (string, error) {
	type result struct {
		line string
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		sc := bufio.NewScanner(r)
		sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if line == "" {
				continue
			}
			if strings.HasPrefix(line, plugin.HandshakePrefix) {
				ch <- result{line: line}
				return
			}
			// Ignore any pre-handshake stdout noise (the SDK emits none).
		}
		if err := sc.Err(); err != nil {
			ch <- result{err: err}
			return
		}
		ch <- result{err: fmt.Errorf("plugin exited before emitting a handshake line")}
	}()

	select {
	case res := <-ch:
		return res.line, res.err
	case <-time.After(timeout):
		return "", fmt.Errorf("timed out after %s waiting for handshake", timeout)
	}
}

// drainAndTerminate asks the plugin to Drain (bounded grace), then stops the process
// (SIGTERM, then SIGKILL after grace) and closes the conn. Safe on a nil entry.
func (h *host) drainAndTerminate(ctx context.Context, e *entry) {
	if e == nil {
		return
	}
	if e.conn != nil {
		dctx, cancel := context.WithTimeout(ctx, plugin.DefaultDrainGrace)
		_, err := pluginv1.NewLifecycleClient(e.conn).Drain(dctx, &pluginv1.DrainRequest{
			GraceSeconds: int64(plugin.DefaultDrainGrace / time.Second),
		})
		cancel()
		if err != nil {
			h.log.V(1).Info("drain rpc failed; terminating anyway", "plugin", e.name, "err", err)
		}
	}
	terminate(e.cmd, plugin.DefaultDrainGrace)
	if e.conn != nil {
		_ = e.conn.Close()
	}
}

// terminate sends SIGTERM and, if the process has not exited within grace, SIGKILL.
// It reaps the process (exactly one cmd.Wait per launched process).
func terminate(cmd *exec.Cmd, grace time.Duration) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = cmd.Process.Signal(syscall.SIGTERM)
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(grace):
		_ = cmd.Process.Kill()
		<-done
	}
}

// killAndReap force-kills and reaps a process on an error path.
func killAndReap(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
}

// resolveDigest computes the trial-environment fingerprint digest for a plugin.
// TODO(m2): replace with the cosign-verified image digest (§5.3, §9). For M0 this is
// a sha256 of the on-disk binary, falling back to the image ref if it can't be read.
func (h *host) resolveDigest(spec launchSpec) string {
	if d, err := fileDigest(spec.binPath); err == nil {
		return d
	} else {
		h.log.V(1).Info("could not hash plugin binary; using image ref as digest",
			"plugin", spec.name, "bin", spec.binPath, "err", err)
	}
	return spec.image
}

func fileDigest(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	sum := sha256.New()
	if _, err := io.Copy(sum, f); err != nil {
		return "", err
	}
	return "sha256:" + hex.EncodeToString(sum.Sum(nil)), nil
}
