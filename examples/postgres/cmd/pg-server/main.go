//go:build linux

// Command pg-server is a privilege-dropping supervisor that starts PostgreSQL
// inside the OpenThesis Firecracker guest (which runs as root).
//
// PostgreSQL refuses to start as root; this binary:
//  1. Creates /etc/passwd + /etc/group with a postgres user (uid/gid 999).
//  2. Copies the pre-initialized data dir from /mnt/sut/pgdata-seed/ to
//     /var/lib/pg/data/ (fast cp, no initdb in the VM) on first boot.
//  3. Sets ownership of the data dir to uid 999.
//  4. Drops to uid 999 and exec(2)s into postgres - same PID, tracked by
//     openthesis-init, so fault injection targets the actual postgres process.
//
// All postgres binaries live on the SUT ext4 image at /mnt/sut/bin/.
// Shared libs are at /mnt/sut/lib/x86_64-linux-gnu/; the openthesis-init
// boot code symlinks them into /lib/x86_64-linux-gnu/ automatically.
package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
)

const (
	pgUID  = 999
	pgGID  = 999
	pgData = "/var/lib/pg/data"
	pgPort = "5432"

	sutBin     = "/mnt/sut/bin"
	sutLib     = "/mnt/sut/lib/x86_64-linux-gnu"
	pgDataSeed = "/mnt/sut/pgdata-seed"
)

func main() {
	// Pin to OS thread so setgid/setuid apply to the thread that calls Exec.
	runtime.LockOSThread()

	setupEtcFiles()

	os.MkdirAll("/var/lib/pg", 0o755)
	os.Chown("/var/lib/pg", pgUID, pgGID)

	if _, err := os.Stat(pgData + "/PG_VERSION"); err != nil {
		if err := copyDataDir(pgDataSeed, pgData); err != nil {
			logf("copy pgdata-seed: %v", err)
			os.Exit(1)
		}
		logf("data dir ready at %s", pgData)
	} else {
		logf("data dir already exists, skipping copy")
	}

	if err := chownR(pgData, pgUID, pgGID); err != nil {
		logf("chown %s: %v (continuing)", pgData, err)
	}

	// Build environment for postgres.
	ldPath := sutLib
	if cur := os.Getenv("LD_LIBRARY_PATH"); cur != "" {
		ldPath = cur + ":" + ldPath
	}
	env := filterEnv(os.Environ())
	env = append(env, "LD_LIBRARY_PATH="+ldPath)
	env = append(env, "PGDATA="+pgData)
	env = append(env, "HOME=/var/lib/pg")

	postgresPath := sutBin + "/postgres"
	args := []string{postgresPath, "-D", pgData, "-p", pgPort}

	// Create the postgres unix socket / lock file directory before dropping privs.
	os.MkdirAll("/var/run/postgresql", 0o775)
	os.Chown("/var/run/postgresql", pgUID, pgGID)

	// Postgres resolves its share directory (timezonesets, tsearch_data, etc.)
	// relative to the binary path → always resolves to the compiled-in pkgdatadir
	// (/usr/share/postgresql/16).  The initramfs has no such directory; create a
	// symlink here (as root, before dropping privs) so postgres can find its data.
	if _, err := os.Lstat("/usr/share/postgresql"); os.IsNotExist(err) {
		os.MkdirAll("/usr/share/postgresql", 0o755)
	}
	if _, err := os.Lstat("/usr/share/postgresql/16"); os.IsNotExist(err) {
		if err := os.Symlink("/mnt/sut/pgshare", "/usr/share/postgresql/16"); err != nil {
			logf("symlink pgshare: %v (continuing)", err)
		}
	}

	// Drop privileges: setgid before setuid (order matters on Linux).
	if err := syscall.Setgid(pgGID); err != nil {
		logf("setgid: %v", err)
		os.Exit(1)
	}
	if err := syscall.Setuid(pgUID); err != nil {
		logf("setuid: %v", err)
		os.Exit(1)
	}

	// Replace this process with postgres. The PID tracked by openthesis-init
	// becomes the postgres process, so terminate_node faults hit postgres.
	if err := syscall.Exec(postgresPath, args, env); err != nil {
		logf("exec postgres: %v", err)
		os.Exit(1)
	}
}

// copyDataDir copies src directory tree to dst using cp -a (preserves perms,
// symlinks, and modification times). Faster than initdb for pre-seeded dirs.
func copyDataDir(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	// Use cp -a via busybox for a faithful recursive copy.
	// Fall back to our own Go copy if cp is unavailable.
	cmd := exec.Command("cp", "-a", src+"/.", dst)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		logf("cp -a failed (%v), using Go copy", err)
		return copyDirGo(src, dst)
	}
	return nil
}

// copyDirGo is a pure-Go recursive directory copy fallback.
func copyDirGo(src, dst string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, path)
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, info.Mode())
		}
		if info.Mode()&os.ModeSymlink != 0 {
			link, _ := os.Readlink(path)
			return os.Symlink(link, target)
		}
		return copyFile(path, target, info.Mode())
	})
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}

// chownR recursively changes ownership of path and all children.
func chownR(path string, uid, gid int) error {
	return filepath.Walk(path, func(p string, _ os.FileInfo, err error) error {
		if err != nil {
			return nil // skip unreadable entries
		}
		return os.Lchown(p, uid, gid)
	})
}

// setupEtcFiles writes minimal /etc/passwd and /etc/group so the postgres
// binary can resolve uid 999 → "postgres" (required by postmaster startup).
func setupEtcFiles() {
	os.MkdirAll("/etc", 0o755)
	passwd := "root:x:0:0:root:/root:/bin/sh\n" +
		"postgres:x:999:999:PostgreSQL:/var/lib/pg:/bin/sh\n"
	if err := os.WriteFile("/etc/passwd", []byte(passwd), 0o644); err != nil {
		logf("write /etc/passwd: %v", err)
	}
	group := "root:x:0:\npostgres:x:999:\n"
	if err := os.WriteFile("/etc/group", []byte(group), 0o644); err != nil {
		logf("write /etc/group: %v", err)
	}
}

// filterEnv removes variables we set ourselves to avoid double-setting.
func filterEnv(env []string) []string {
	out := env[:0:len(env)]
	for _, e := range env {
		if strings.HasPrefix(e, "LD_LIBRARY_PATH=") ||
			strings.HasPrefix(e, "PGDATA=") ||
			strings.HasPrefix(e, "HOME=") {
			continue
		}
		out = append(out, e)
	}
	return out
}

func logf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "[pg-server] "+format+"\n", args...)
}
