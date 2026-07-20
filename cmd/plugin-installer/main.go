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

// Command plugin-installer is a one-shot CLI that copies first-party plugin binaries
// into the host's plugin directory (DESIGN.md §5.2 discovery). It is the kapture
// init-container style installer: an OCI plugin image bundles the parallax-* binaries
// and its entrypoint runs this to populate a shared /plugins volume the operator then
// discovers.
//
// It is NOT a plugin: it takes plain --src/--plugin-dir flags and exits. Each copy is
// atomic (temp file in the destination dir + rename) so a concurrent discovery never
// observes a half-written binary. Logs go to stderr.
package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
)

func main() {
	log.SetOutput(os.Stderr)
	log.SetPrefix("plugin-installer: ")
	log.SetFlags(0)

	var src, pluginDir string
	flag.StringVar(&src, "src", "", "source directory containing parallax-* plugin binaries")
	flag.StringVar(&pluginDir, "plugin-dir", "", "destination plugin directory (the host PluginDir)")
	flag.Parse()

	if src == "" || pluginDir == "" {
		flag.Usage()
		log.Fatal("both --src and --plugin-dir are required")
	}

	if err := run(src, pluginDir); err != nil {
		log.Fatal(err)
	}
}

// run installs every parallax-* file from src into pluginDir.
func run(src, pluginDir string) error {
	entries, err := os.ReadDir(src)
	if err != nil {
		return fmt.Errorf("read source dir %s: %w", src, err)
	}
	if err := os.MkdirAll(pluginDir, 0o755); err != nil {
		return fmt.Errorf("create plugin dir %s: %w", pluginDir, err)
	}

	installed := 0
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		// AppleDouble sidecars on exFAT volumes are not plugins.
		if strings.HasPrefix(name, "._") {
			continue
		}
		if !strings.HasPrefix(name, "parallax-") {
			continue
		}
		srcPath := filepath.Join(src, name)
		dstPath := filepath.Join(pluginDir, name)
		if err := installFile(srcPath, dstPath); err != nil {
			return fmt.Errorf("install %s: %w", name, err)
		}
		log.Printf("installed %s -> %s", name, dstPath)
		installed++
	}

	if installed == 0 {
		return fmt.Errorf("no parallax-* files found in %s", src)
	}
	log.Printf("installed %d plugin binary(ies) into %s", installed, pluginDir)
	return nil
}

// installFile atomically copies src to dst: it writes to a temp file in the
// destination directory, fsyncs, chmods 0755, then renames over the final path so a
// concurrent reader never sees a partial binary. rename within one directory is
// atomic on POSIX filesystems.
func installFile(src, dst string) (retErr error) {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open source: %w", err)
	}
	defer in.Close()

	tmp, err := os.CreateTemp(filepath.Dir(dst), ".parallax-install-*")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpName := tmp.Name()
	// Best-effort cleanup if we bail before the rename; a no-op after a successful
	// rename (the temp path no longer exists).
	defer func() {
		if retErr != nil {
			_ = os.Remove(tmpName)
		}
	}()

	if _, err := io.Copy(tmp, in); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("copy: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp file: %w", err)
	}
	if err := os.Chmod(tmpName, 0o755); err != nil {
		return fmt.Errorf("chmod 0755: %w", err)
	}
	if err := os.Rename(tmpName, dst); err != nil {
		return fmt.Errorf("rename %s -> %s: %w", tmpName, dst, err)
	}
	return nil
}
