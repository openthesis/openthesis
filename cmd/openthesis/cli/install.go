package cli

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
)

// CmdInstall implements `openthesis install`.
// It locates deploy/local/install relative to the binary, the source tree,
// or $OPENTHESIS_DEPLOY_DIR and execs it so that all TUI/logging is handled
// by gum in the shell scripts.
func CmdInstall(args []string) int {
	fs := flag.NewFlagSet("install", flag.ExitOnError)
	backendFlag := fs.String("backend", "", "backend to install for: tcg or firecracker (default: firecracker on Linux, tcg on macOS)")
	force := fs.Bool("force", false, "re-build / re-download artifacts even if they already exist")
	buildFC := fs.Bool("build-firecracker", false, "build patched Firecracker from source instead of downloading pre-built")
	buildKernel := fs.Bool("build-kernel", false, "build guest kernel from source instead of downloading pre-built")
	hostKernel := fs.Bool("host-kernel", false, "also patch host KVM modules for RDTSC+HLT exits (Linux/Firecracker only, requires sudo)")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: openthesis install [flags]\n\n")
		fmt.Fprintf(os.Stderr, "Download or build all runtime artifacts to ~/.openthesis/local/.\n")
		fmt.Fprintf(os.Stderr, "Pre-built artifacts are downloaded from GitHub Releases when available.\n\n")
		fmt.Fprintf(os.Stderr, "Flags:\n")
		fs.PrintDefaults()
	}
	fs.Parse(args) //nolint:errcheck

	backend := *backendFlag
	if backend == "" {
		if runtime.GOOS == "linux" {
			backend = "firecracker"
		} else {
			backend = "tcg"
		}
	}
	if backend != "tcg" && backend != "firecracker" {
		fmt.Fprintf(os.Stderr, "error: --backend must be tcg or firecracker\n")
		return 1
	}

	script := findInstallScript()
	if script == "" {
		fmt.Fprintf(os.Stderr, "error: deploy/local/install not found\n")
		fmt.Fprintf(os.Stderr, "openthesis install requires the source tree.\n")
		fmt.Fprintf(os.Stderr, "Run: make install\n")
		fmt.Fprintf(os.Stderr, "Or:  git clone https://github.com/openthesis/openthesis && cd openthesis && make install\n")
		return 1
	}

	env := os.Environ()
	env = setEnv(env, "BACKEND", backend)
	if *force {
		env = setEnv(env, "FORCE", "1")
	}
	if *buildFC {
		env = setEnv(env, "BUILD_FIRECRACKER", "1")
	}
	if *buildKernel {
		env = setEnv(env, "BUILD_KERNEL", "1")
	}
	if *hostKernel {
		env = setEnv(env, "HOST_KERNEL", "1")
	}

	cmd := exec.Command("bash", script)
	cmd.Env = env
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		exitErr := &exec.ExitError{}
		if errors.As(err, &exitErr) {
			return exitErr.ExitCode()
		}
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	return 0
}

// findInstallScript searches for deploy/local/install in several locations:
// 1. $OPENTHESIS_DEPLOY_DIR/local/install
// 2. Adjacent to the running binary: <binary>/../deploy/local/install
// 3. Relative to the working directory: ./deploy/local/install
func findInstallScript() string {
	const rel = "local/install"

	if dir := os.Getenv("OPENTHESIS_DEPLOY_DIR"); dir != "" {
		p := filepath.Join(dir, rel)
		if isFile(p) {
			return p
		}
	}

	if exe, err := os.Executable(); err == nil {
		p := filepath.Join(filepath.Dir(exe), "..", "deploy", rel)
		if isFile(p) {
			return p
		}
		// Resolved symlink path (common in go install scenarios).
		if resolved, err := filepath.EvalSymlinks(exe); err == nil {
			p = filepath.Join(filepath.Dir(resolved), "..", "deploy", rel)
			if isFile(p) {
				return p
			}
		}
	}

	if cwd, err := os.Getwd(); err == nil {
		p := filepath.Join(cwd, "deploy", rel)
		if isFile(p) {
			return p
		}
	}

	return ""
}

func isFile(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && !fi.IsDir()
}

// setEnv replaces or appends KEY=val in an environ slice.
func setEnv(env []string, key, val string) []string {
	prefix := key + "="
	for i, e := range env {
		if len(e) >= len(prefix) && e[:len(prefix)] == prefix {
			env[i] = prefix + val
			return env
		}
	}
	return append(env, prefix+val)
}
