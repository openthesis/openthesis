package container

import (
	"archive/tar"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// ImportImage exports a Docker image and extracts its layers into a flat
// rootfs directory suitable for overlay onto the base VM rootfs.
func ImportImage(image, destDir string) (*ImageConfig, error) {
	tmpDir, err := os.MkdirTemp("", "openthesis-import-*")
	if err != nil {
		return nil, fmt.Errorf("import tmp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	tarPath := filepath.Join(tmpDir, "image.tar")

	slog.Info("container: exporting image", "image", image)
	cmd := exec.Command("docker", "save", image, "-o", tarPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("docker save %s: %s: %w", image, string(out), err)
	}

	f, err := os.Open(tarPath)
	if err != nil {
		return nil, fmt.Errorf("import open tar: %w", err)
	}
	defer f.Close()

	manifest, layers, err := readManifest(f)
	if err != nil {
		return nil, fmt.Errorf("import read manifest: %w", err)
	}

	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return nil, fmt.Errorf("import mkdir dest: %w", err)
	}

	for _, layerPath := range layers {
		slog.Debug("container: extracting layer", "layer", layerPath)
		if err := extractLayer(tarPath, layerPath, destDir); err != nil {
			return nil, fmt.Errorf("import extract layer %s: %w", layerPath, err)
		}
	}

	cfg, err := readImageConfig(tarPath, manifest.Config)
	if err != nil {
		slog.Warn("container: failed to read image config", "err", err)
		return &ImageConfig{}, nil
	}

	slog.Info("container: image imported",
		"image", image,
		"layers", len(layers),
		"entrypoint", cfg.Entrypoint,
		"cmd", cfg.Cmd,
	)

	return cfg, nil
}

// ImageConfig holds the runtime configuration extracted from a Docker image.
type ImageConfig struct {
	Entrypoint []string          `json:"entrypoint,omitempty"`
	Cmd        []string          `json:"cmd,omitempty"`
	Env        map[string]string `json:"env,omitempty"`
	WorkingDir string            `json:"working_dir,omitempty"`
}

// Command returns the effective command to run (entrypoint + cmd).
func (c *ImageConfig) Command() []string {
	if len(c.Entrypoint) > 0 {
		return append(c.Entrypoint, c.Cmd...)
	}
	return c.Cmd
}

type dockerManifest struct {
	Config   string   `json:"Config"`
	RepoTags []string `json:"RepoTags"`
	Layers   []string `json:"Layers"`
}

func readManifest(r io.ReadSeeker) (dockerManifest, []string, error) {
	tr := tar.NewReader(r)

	var manifestData []byte
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return dockerManifest{}, nil, err
		}
		if hdr.Name == "manifest.json" {
			manifestData, err = io.ReadAll(tr)
			if err != nil {
				return dockerManifest{}, nil, err
			}
			break
		}
	}

	if manifestData == nil {
		return dockerManifest{}, nil, fmt.Errorf("manifest.json not found in image")
	}

	var manifests []dockerManifest
	if err := json.Unmarshal(manifestData, &manifests); err != nil {
		return dockerManifest{}, nil, err
	}
	if len(manifests) == 0 {
		return dockerManifest{}, nil, fmt.Errorf("empty manifest")
	}

	m := manifests[0]
	return m, m.Layers, nil
}

func extractLayer(tarPath, layerPath, destDir string) error {
	f, err := os.Open(tarPath)
	if err != nil {
		return err
	}
	defer f.Close()

	tr := tar.NewReader(f)

	// Find the layer tar within the image tar.
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return fmt.Errorf("layer %s not found", layerPath)
		}
		if err != nil {
			return err
		}
		if hdr.Name == layerPath {
			return extractTar(tr, destDir)
		}
	}
}

func extractTar(r io.Reader, destDir string) error {
	tr := tar.NewReader(r)

	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}

		// Handle whiteout files (Docker's layer deletion markers).
		name := hdr.Name
		base := filepath.Base(name)
		if strings.HasPrefix(base, ".wh.") {
			target := filepath.Join(destDir, filepath.Dir(name), strings.TrimPrefix(base, ".wh."))
			os.RemoveAll(target)
			continue
		}

		target := filepath.Join(destDir, filepath.Clean(name))

		// Prevent path traversal.
		if !strings.HasPrefix(target, filepath.Clean(destDir)+string(os.PathSeparator)) && target != filepath.Clean(destDir) {
			continue
		}

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, os.FileMode(hdr.Mode)); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(hdr.Mode))
			if err != nil {
				return err
			}
			if _, err := io.Copy(out, tr); err != nil {
				out.Close()
				return err
			}
			out.Close()
		case tar.TypeSymlink:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			os.Remove(target)
			if err := os.Symlink(hdr.Linkname, target); err != nil {
				return err
			}
		case tar.TypeLink:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			linkTarget := filepath.Join(destDir, filepath.Clean(hdr.Linkname))
			os.Remove(target)
			if err := os.Link(linkTarget, target); err != nil {
				// Fall back to copy if hard link fails (cross-device).
				copyFile(linkTarget, target)
			}
		}
	}
}

func readImageConfig(tarPath, configPath string) (*ImageConfig, error) {
	f, err := os.Open(tarPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	tr := tar.NewReader(f)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil, fmt.Errorf("config %s not found", configPath)
		}
		if err != nil {
			return nil, err
		}
		if hdr.Name == configPath {
			data, err := io.ReadAll(tr)
			if err != nil {
				return nil, err
			}

			var raw struct {
				Config struct {
					Entrypoint []string `json:"Entrypoint"`
					Cmd        []string `json:"Cmd"`
					Env        []string `json:"Env"`
					WorkingDir string   `json:"WorkingDir"`
				} `json:"config"`
			}
			if err := json.Unmarshal(data, &raw); err != nil {
				return nil, err
			}

			env := make(map[string]string, len(raw.Config.Env))
			for _, e := range raw.Config.Env {
				k, v, _ := strings.Cut(e, "=")
				env[k] = v
			}

			return &ImageConfig{
				Entrypoint: raw.Config.Entrypoint,
				Cmd:        raw.Config.Cmd,
				Env:        env,
				WorkingDir: raw.Config.WorkingDir,
			}, nil
		}
	}
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()

	_, err = io.Copy(out, in)
	return err
}
