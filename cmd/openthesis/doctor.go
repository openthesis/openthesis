package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// cmdDoctor implements `openthesis doctor`.
//
// It validates that the host environment is ready to run OpenThesis campaigns:
// checks KVM access, required binaries, disk space, vsock, and permissions.
// Each check prints ✓ or ✗ with a one-line explanation and a fix command for
// failures. Exit code 0 if all checks pass, 1 if any fail.
func cmdDoctor(args []string) int {
	fs := flag.NewFlagSet("doctor", flag.ExitOnError)
	backend := fs.String("backend", "", "check prerequisites for a specific backend (firecracker, tcg, gvisor, patched)")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: openthesis doctor [--backend <backend>]\n\n")
		fmt.Fprintf(os.Stderr, "Check that the host environment is ready to run OpenThesis.\n\n")
		fmt.Fprintf(os.Stderr, "Flags:\n")
		fs.PrintDefaults()
	}
	fs.Parse(args) //nolint:errcheck

	fmt.Println()
	fmt.Println("  Checking OpenThesis prerequisites...")
	fmt.Println()

	var checks []check
	checks = append(checks, commonChecks()...)

	switch *backend {
	case "firecracker", "":
		checks = append(checks, firecrackerChecks()...)
	case "tcg", "patched":
		checks = append(checks, qemuChecks()...)
	case "gvisor":
		checks = append(checks, gvisorChecks()...)
	}

	pass, fail := 0, 0
	for _, c := range checks {
		ok, detail, fix := c.run()
		if ok {
			fmt.Printf("  %s  %s\n", green("✓"), c.name)
			if detail != "" {
				fmt.Printf("       %s\n", dim(detail))
			}
			pass++
		} else {
			fmt.Printf("  %s  %s\n", red("✗"), c.name)
			if detail != "" {
				fmt.Printf("       %s\n", dim(detail))
			}
			if fix != "" {
				fmt.Printf("       %s %s\n", dim("fix:"), yellow(fix))
			}
			fail++
		}
	}

	fmt.Println()
	if fail == 0 {
		fmt.Printf("  %s All checks passed. Ready to run.\n\n", green("✓"))
		fmt.Printf("  Next step:\n")
		fmt.Printf("    openthesis run --config openthesis.json --backend firecracker\n\n")
		return 0
	}
	plural := "issue"
	if fail > 1 {
		plural = "issues"
	}
	fmt.Printf("  %s %d %s found. Fix the %s above and re-run `openthesis doctor`.\n\n",
		red("✗"), fail, plural, plural)
	return 1
}

type check struct {
	name string
	run  func() (ok bool, detail string, fix string)
}

// commonChecks are backend-independent checks that always run.
func commonChecks() []check {
	return []check{
		{
			name: "openthesis-init binary",
			run: func() (bool, string, string) {
				p := defaultInitBinary()
				if _, err := os.Stat(p); err == nil {
					return true, p, ""
				}
				// Also try PATH.
				if found, err := exec.LookPath("openthesis-init"); err == nil {
					return true, found, ""
				}
				return false,
					"not found at " + p,
					"build and deploy: make install  or  run ./deploy/deploy.sh <user>@<server>"
			},
		},
		{
			name: "guest kernel image",
			run: func() (bool, string, string) {
				p := defaultKernelPath()
				if p != "" {
					return true, p, ""
				}
				return false,
					"not found in ~/.openthesis/kernel/ or ~/.openthesis/local/<arch>/kernel/",
					"build with: ./deploy/build-kernel.sh <user>@<server>"
			},
		},
		{
			name: "state directory writable",
			run: func() (bool, string, string) {
				dir := defaultStateDir()
				if err := os.MkdirAll(dir, 0o750); err != nil {
					return false,
						"cannot create " + dir + ": " + err.Error(),
						"fix permissions on " + filepath.Dir(dir)
				}
				tmp := filepath.Join(dir, ".doctor-probe")
				if err := os.WriteFile(tmp, []byte("probe"), 0o600); err != nil {
					return false,
						dir + " is not writable: " + err.Error(),
						"run: chmod u+rwx " + dir
				}
				os.Remove(tmp) //nolint:errcheck
				return true, dir, ""
			},
		},
	}
}

// firecrackerChecks validates the Firecracker backend specifically.
func firecrackerChecks() []check {
	return []check{
		{
			name: "/dev/kvm - KVM access",
			run: func() (bool, string, string) {
				fi, err := os.Stat("/dev/kvm")
				if os.IsNotExist(err) {
					return false,
						"/dev/kvm not found",
						"enable KVM: modprobe kvm_intel  (or kvm_amd) - or enable virtualisation in BIOS"
				}
				if err != nil {
					return false, err.Error(), ""
				}
				// Check read/write permission.
				f, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0)
				if err != nil {
					return false,
						"not readable by current user",
						"run: sudo usermod -aG kvm $USER  then log out and back in"
				}
				f.Close()
				return true, fi.Name(), ""
			},
		},
		{
			name: "/dev/vsock - vsock support",
			run: func() (bool, string, string) {
				if _, err := os.Stat("/dev/vsock"); err == nil {
					return true, "/dev/vsock", ""
				}
				return false,
					"/dev/vsock not found",
					"run: sudo modprobe vhost_vsock  (and add vhost_vsock to /etc/modules)"
			},
		},
		{
			name: "firecracker binary",
			run: func() (bool, string, string) {
				p := defaultFirecracker()
				if p != "firecracker" {
					if _, err := os.Stat(p); err == nil {
						v := fcVersion(p)
						return true, p + v, ""
					}
				}
				if found, err := exec.LookPath("firecracker"); err == nil {
					v := fcVersion(found)
					return true, found + v, ""
				}
				return false,
					"not found in ~/.openthesis/bin/ - build with: BUILD_FIRECRACKER=1 make install",
					"build patched firecracker: ./deploy/build-firecracker.sh <user>@<server>"
			},
		},
		{
			name: "/dev/shm - shared memory for snapshots",
			run: func() (bool, string, string) {
				var st syscall.Statfs_t
				if err := syscall.Statfs("/dev/shm", &st); err != nil {
					return false, "cannot stat /dev/shm: " + err.Error(), ""
				}
				freeGB := float64(st.Bavail) * float64(st.Bsize) / 1e9
				if freeGB < 2.0 {
					return false,
						fmt.Sprintf("only %.1f GB free - need ≥ 2 GB per parallel VM", freeGB),
						"free space: sudo rm -rf /dev/shm/firecracker-*  or reduce --parallel"
				}
				return true, fmt.Sprintf("%.0f GB available", freeGB), ""
			},
		},
		{
			name: "root or CAP_NET_ADMIN - TAP interface creation",
			run: func() (bool, string, string) {
				if os.Getuid() == 0 {
					return true, "running as root", ""
				}
				// Check if user has CAP_NET_ADMIN via /proc/self/status.
				if hasCapNetAdmin() {
					return true, "CAP_NET_ADMIN granted", ""
				}
				return false,
					"TAP interfaces require elevated privileges",
					"run openthesis as root: sudo openthesis run ...  or grant CAP_NET_ADMIN via sudo capabilities"
			},
		},
	}
}

// qemuChecks validates the TCG / patched-QEMU backend.
func qemuChecks() []check {
	return []check{
		{
			name: "qemu-system-x86_64 binary",
			run: func() (bool, string, string) {
				p := defaultQEMU()
				if p != "qemu-system-x86_64" {
					if _, err := os.Stat(p); err == nil {
						return true, p, ""
					}
				}
				if found, err := exec.LookPath("qemu-system-x86_64"); err == nil {
					return true, found, ""
				}
				return false,
					"not found",
					"install: apt install qemu-system-x86  or  ./deploy/build-qemu.sh <user>@<server>"
			},
		},
	}
}

// gvisorChecks validates the gVisor backend.
func gvisorChecks() []check {
	return []check{
		{
			name: "runsc binary",
			run: func() (bool, string, string) {
				p := defaultRunsc()
				if p != "runsc" {
					if _, err := os.Stat(p); err == nil {
						return true, p, ""
					}
				}
				if found, err := exec.LookPath("runsc"); err == nil {
					return true, found, ""
				}
				return false,
					"not found",
					"build patched runsc: ./deploy/build-gvisor.sh <user>@<server>"
			},
		},
	}
}

func fcVersion(p string) string {
	out, err := exec.Command(p, "--version").Output()
	if err != nil {
		return ""
	}
	line := strings.TrimSpace(string(out))
	// "Firecracker v1.15.0" → "(v1.15.0)"
	parts := strings.Fields(line)
	if len(parts) >= 2 {
		return " (" + parts[len(parts)-1] + ")"
	}
	return ""
}

func hasCapNetAdmin() bool {
	data, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, "CapEff:") {
			continue
		}
		hexStr := strings.TrimSpace(strings.TrimPrefix(line, "CapEff:"))
		caps, err := strconv.ParseUint(hexStr, 16, 64)
		if err != nil {
			return false
		}
		const capNetAdmin = 12
		return (caps>>capNetAdmin)&1 == 1
	}
	return false
}

func green(s string) string {
	if !isTerminal(os.Stderr) {
		return s
	}
	return "\x1b[32m" + s + "\x1b[0m"
}

func red(s string) string {
	if !isTerminal(os.Stderr) {
		return s
	}
	return "\x1b[31m" + s + "\x1b[0m"
}

func yellow(s string) string {
	if !isTerminal(os.Stderr) {
		return s
	}
	return "\x1b[33m" + s + "\x1b[0m"
}

func dim(s string) string {
	if !isTerminal(os.Stderr) {
		return s
	}
	return "\x1b[2m" + s + "\x1b[0m"
}
