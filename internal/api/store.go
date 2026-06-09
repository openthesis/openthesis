package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

var (
	errNotFound = errors.New("not found")
)

// Store persists all resources as atomic JSON files under a root directory.
//
// Layout:
//
//	$root/settings.json
//	$root/apikeys/{kid}.json
//	$root/projects/{pid}/project.json
//	$root/projects/{pid}/environments/{eid}/environment.json
//	$root/projects/{pid}/tests/{tid}/test.json
//	$root/projects/{pid}/tests/{tid}/runs/{rid}/run.json
//	$root/projects/{pid}/tests/{tid}/findings/{fid}.json
//	$root/projects/{pid}/tests/{tid}/assertions/{aid}.json
//	$root/projects/{pid}/tests/{tid}/coverage.json
//	$root/projects/{pid}/tests/{tid}/reports/{rid}.json
//	$root/projects/{pid}/webhooks/{wid}.json
//	$root/projects/{pid}/webhooks/deliveries/{did}.json
//	$root/projects/{pid}/replays/{rid}.json
//	$root/projects/{pid}/notebooks/{nid}.json
type Store struct {
	root string
}

func newStore(root string) (*Store, error) {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, fmt.Errorf("create state dir: %w", err)
	}
	return &Store{root: root}, nil
}

// atomicWriteJSON atomically writes v as JSON to path.
func atomicWriteJSON(path string, v any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("mkdir: %w", err)
	}
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return fmt.Errorf("create tmp: %w", err)
	}
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	if encErr := enc.Encode(v); encErr != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("encode: %w", encErr)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("close: %w", err)
	}
	return os.Rename(tmp, path)
}

// readJSON decodes the JSON file at path into v.
func readJSON(path string, v any) error {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return errNotFound
		}
		return err
	}
	defer f.Close()
	return json.NewDecoder(f).Decode(v)
}

// removeFile removes a file, ignoring not-found errors.
func removeFile(path string) error {
	err := os.Remove(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// listDir returns the names of all files with the given suffix in dir.
func listDir(dir, suffix string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), suffix) {
			names = append(names, filepath.Join(dir, e.Name()))
		}
	}
	return names, nil
}

// listSubdirs returns paths of immediate subdirectories in dir.
func listSubdirs(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var dirs []string
	for _, e := range entries {
		if e.IsDir() {
			dirs = append(dirs, filepath.Join(dir, e.Name()))
		}
	}
	return dirs, nil
}

func readObject[T any](path string) (T, error) {
	var out T
	if err := readJSON(path, &out); err != nil {
		return out, err
	}
	return out, nil
}

func readObjectsFromFiles[T any](paths []string, include func(string) bool) []T {
	out := make([]T, 0, len(paths))
	for _, path := range paths {
		if include != nil && !include(path) {
			continue
		}
		v, err := readObject[T](path)
		if err != nil {
			continue
		}
		out = append(out, v)
	}
	return out
}

func readObjectsFromSubdirs[T any](dirs []string, filename string) []T {
	out := make([]T, 0, len(dirs))
	for _, dir := range dirs {
		v, err := readObject[T](filepath.Join(dir, filename))
		if err != nil {
			continue
		}
		out = append(out, v)
	}
	return out
}

func (s *Store) GetSettings() (Settings, error) {
	var v Settings
	err := readJSON(filepath.Join(s.root, "settings.json"), &v)
	if errors.Is(err, errNotFound) {
		return defaultSettings(), nil
	}
	return v, err
}

func (s *Store) SaveSettings(v Settings) error {
	return atomicWriteJSON(filepath.Join(s.root, "settings.json"), v)
}

type storedAPIKey struct {
	APIKey
	Hash string `json:"hash"`
}

func (s *Store) SaveAPIKey(k storedAPIKey) error {
	return atomicWriteJSON(filepath.Join(s.root, "apikeys", k.ID+".json"), k)
}

func (s *Store) GetAPIKey(id string) (storedAPIKey, error) {
	var v storedAPIKey
	err := readJSON(filepath.Join(s.root, "apikeys", id+".json"), &v)
	return v, err
}

func (s *Store) ListAPIKeys() ([]APIKey, error) {
	paths, err := listDir(filepath.Join(s.root, "apikeys"), ".json")
	if err != nil {
		return nil, err
	}
	stored := readObjectsFromFiles[storedAPIKey](paths, nil)
	out := make([]APIKey, 0, len(stored))
	for _, k := range stored {
		out = append(out, k.APIKey)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out, nil
}

func (s *Store) DeleteAPIKey(id string) error {
	return removeFile(filepath.Join(s.root, "apikeys", id+".json"))
}

// FindAPIKeyByHash returns the stored key whose hash matches.
func (s *Store) FindAPIKeyByHash(hash string) (storedAPIKey, error) {
	paths, err := listDir(filepath.Join(s.root, "apikeys"), ".json")
	if err != nil {
		return storedAPIKey{}, err
	}
	for _, p := range paths {
		var v storedAPIKey
		if readJSON(p, &v) != nil {
			continue
		}
		if v.Hash == hash {
			return v, nil
		}
	}
	return storedAPIKey{}, errNotFound
}

func (s *Store) projectDir(pid string) string {
	return filepath.Join(s.root, "projects", pid)
}

func (s *Store) SaveProject(p Project) error {
	return atomicWriteJSON(filepath.Join(s.projectDir(p.ID), "project.json"), p)
}

func (s *Store) GetProject(pid string) (Project, error) {
	var v Project
	err := readJSON(filepath.Join(s.projectDir(pid), "project.json"), &v)
	return v, err
}

func (s *Store) ListProjects() ([]Project, error) {
	dirs, err := listSubdirs(filepath.Join(s.root, "projects"))
	if err != nil {
		return nil, err
	}
	out := readObjectsFromSubdirs[Project](dirs, "project.json")
	sort.Slice(out, func(i, j int) bool {
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out, nil
}

func (s *Store) DeleteProject(pid string) error {
	return os.RemoveAll(s.projectDir(pid))
}

func (s *Store) envDir(pid, eid string) string {
	return filepath.Join(s.projectDir(pid), "environments", eid)
}

func (s *Store) SaveEnvironment(e Environment) error {
	return atomicWriteJSON(filepath.Join(s.envDir(e.ProjectID, e.ID), "environment.json"), e)
}

func (s *Store) GetEnvironment(pid, eid string) (Environment, error) {
	var v Environment
	err := readJSON(filepath.Join(s.envDir(pid, eid), "environment.json"), &v)
	return v, err
}

func (s *Store) ListEnvironments(pid string) ([]Environment, error) {
	dirs, err := listSubdirs(filepath.Join(s.projectDir(pid), "environments"))
	if err != nil {
		return nil, err
	}
	out := readObjectsFromSubdirs[Environment](dirs, "environment.json")
	sort.Slice(out, func(i, j int) bool {
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out, nil
}

func (s *Store) DeleteEnvironment(pid, eid string) error {
	return os.RemoveAll(s.envDir(pid, eid))
}

func (s *Store) testDir(pid, tid string) string {
	return filepath.Join(s.projectDir(pid), "tests", tid)
}

func (s *Store) SaveTest(t Test) error {
	return atomicWriteJSON(filepath.Join(s.testDir(t.ProjectID, t.ID), "test.json"), t)
}

func (s *Store) GetTest(pid, tid string) (Test, error) {
	var v Test
	err := readJSON(filepath.Join(s.testDir(pid, tid), "test.json"), &v)
	return v, err
}

func (s *Store) ListTests(pid string) ([]Test, error) {
	dirs, err := listSubdirs(filepath.Join(s.projectDir(pid), "tests"))
	if err != nil {
		return nil, err
	}
	out := readObjectsFromSubdirs[Test](dirs, "test.json")
	sort.Slice(out, func(i, j int) bool {
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out, nil
}

func (s *Store) DeleteTest(pid, tid string) error {
	return os.RemoveAll(s.testDir(pid, tid))
}

func (s *Store) runDir(pid, tid, rid string) string {
	return filepath.Join(s.testDir(pid, tid), "runs", rid)
}

func (s *Store) SaveRun(r Run) error {
	return atomicWriteJSON(filepath.Join(s.runDir(r.ProjectID, r.TestID, r.ID), "run.json"), r)
}

func (s *Store) GetRun(pid, tid, rid string) (Run, error) {
	var v Run
	err := readJSON(filepath.Join(s.runDir(pid, tid, rid), "run.json"), &v)
	return v, err
}

func (s *Store) ListRuns(pid, tid string) ([]Run, error) {
	dirs, err := listSubdirs(filepath.Join(s.testDir(pid, tid), "runs"))
	if err != nil {
		return nil, err
	}
	out := readObjectsFromSubdirs[Run](dirs, "run.json")
	sort.Slice(out, func(i, j int) bool {
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out, nil
}

func (s *Store) findingsDir(pid, tid string) string {
	return filepath.Join(s.testDir(pid, tid), "findings")
}

func (s *Store) SaveFinding(f Finding) error {
	return atomicWriteJSON(filepath.Join(s.findingsDir(f.ProjectID, f.TestID), f.ID+".json"), f)
}

func (s *Store) GetFinding(pid, tid, fid string) (Finding, error) {
	var v Finding
	err := readJSON(filepath.Join(s.findingsDir(pid, tid), fid+".json"), &v)
	return v, err
}

func (s *Store) ListFindings(pid, tid string) ([]Finding, error) {
	paths, err := listDir(s.findingsDir(pid, tid), ".json")
	if err != nil {
		return nil, err
	}
	out := readObjectsFromFiles[Finding](paths, nil)
	sort.Slice(out, func(i, j int) bool {
		return out[i].FirstSeenAt.Before(out[j].FirstSeenAt)
	})
	return out, nil
}

func (s *Store) DeleteFinding(pid, tid, fid string) error {
	return removeFile(filepath.Join(s.findingsDir(pid, tid), fid+".json"))
}

// FindFindingByFingerprint returns the first finding with the given fingerprint, or errNotFound.
func (s *Store) FindFindingByFingerprint(pid, tid, fp string) (Finding, error) {
	paths, err := listDir(s.findingsDir(pid, tid), ".json")
	if err != nil {
		return Finding{}, err
	}
	for _, p := range paths {
		var f Finding
		if readJSON(p, &f) != nil {
			continue
		}
		if f.Fingerprint == fp {
			return f, nil
		}
	}
	return Finding{}, errNotFound
}

func (s *Store) assertionsDir(pid, tid string) string {
	return filepath.Join(s.testDir(pid, tid), "assertions")
}

func (s *Store) SaveAssertion(pid string, a Assertion) error {
	return atomicWriteJSON(filepath.Join(s.assertionsDir(pid, a.TestID), a.ID+".json"), a)
}

func (s *Store) GetAssertion(pid, tid, aid string) (Assertion, error) {
	var v Assertion
	err := readJSON(filepath.Join(s.assertionsDir(pid, tid), aid+".json"), &v)
	return v, err
}

func (s *Store) ListAssertions(pid, tid string) ([]Assertion, error) {
	paths, err := listDir(s.assertionsDir(pid, tid), ".json")
	if err != nil {
		return nil, err
	}
	return readObjectsFromFiles[Assertion](paths, nil), nil
}

func (s *Store) SaveCoverage(pid string, c Coverage) error {
	return atomicWriteJSON(filepath.Join(s.testDir(pid, c.TestID), "coverage.json"), c)
}

func (s *Store) GetCoverage(pid, tid string) (Coverage, error) {
	var v Coverage
	err := readJSON(filepath.Join(s.testDir(pid, tid), "coverage.json"), &v)
	if errors.Is(err, errNotFound) {
		return Coverage{TestID: tid}, nil
	}
	return v, err
}

func (s *Store) reportsDir(pid, tid string) string {
	return filepath.Join(s.testDir(pid, tid), "reports")
}

func (s *Store) SaveReport(r ReportResource) error {
	return atomicWriteJSON(filepath.Join(s.reportsDir(r.ProjectID, r.TestID), r.ID+".json"), r)
}

func (s *Store) GetReport(pid, tid, rid string) (ReportResource, error) {
	var v ReportResource
	err := readJSON(filepath.Join(s.reportsDir(pid, tid), rid+".json"), &v)
	return v, err
}

func (s *Store) ListReports(pid, tid string) ([]ReportResource, error) {
	paths, err := listDir(s.reportsDir(pid, tid), ".json")
	if err != nil {
		return nil, err
	}
	out := readObjectsFromFiles[ReportResource](paths, nil)
	sort.Slice(out, func(i, j int) bool {
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out, nil
}

func (s *Store) webhooksDir(pid string) string {
	return filepath.Join(s.projectDir(pid), "webhooks")
}

func (s *Store) SaveWebhook(w Webhook) error {
	return atomicWriteJSON(filepath.Join(s.webhooksDir(w.ProjectID), w.ID+".json"), w)
}

func (s *Store) GetWebhook(pid, wid string) (Webhook, error) {
	var v Webhook
	err := readJSON(filepath.Join(s.webhooksDir(pid), wid+".json"), &v)
	return v, err
}

func (s *Store) ListWebhooks(pid string) ([]Webhook, error) {
	paths, err := listDir(s.webhooksDir(pid), ".json")
	if err != nil {
		return nil, err
	}
	out := readObjectsFromFiles[Webhook](paths, func(path string) bool {
		return !strings.Contains(path, "deliveries")
	})
	sort.Slice(out, func(i, j int) bool {
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out, nil
}

func (s *Store) DeleteWebhook(pid, wid string) error {
	return removeFile(filepath.Join(s.webhooksDir(pid), wid+".json"))
}

func (s *Store) SaveWebhookDelivery(pid string, d WebhookDelivery) error {
	return atomicWriteJSON(filepath.Join(s.webhooksDir(pid), "deliveries", d.ID+".json"), d)
}

func (s *Store) ListWebhookDeliveries(pid, wid string) ([]WebhookDelivery, error) {
	paths, err := listDir(filepath.Join(s.webhooksDir(pid), "deliveries"), ".json")
	if err != nil {
		return nil, err
	}
	all := readObjectsFromFiles[WebhookDelivery](paths, nil)
	out := make([]WebhookDelivery, 0, len(all))
	for _, v := range all {
		if v.WebhookID == wid {
			out = append(out, v)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out, nil
}

func (s *Store) replaysDir(pid string) string {
	return filepath.Join(s.projectDir(pid), "replays")
}

func (s *Store) SaveReplay(r Replay) error {
	return atomicWriteJSON(filepath.Join(s.replaysDir(r.ProjectID), r.ID+".json"), r)
}

func (s *Store) GetReplay(pid, rid string) (Replay, error) {
	var v Replay
	err := readJSON(filepath.Join(s.replaysDir(pid), rid+".json"), &v)
	return v, err
}

func (s *Store) ListReplays(pid string) ([]Replay, error) {
	paths, err := listDir(s.replaysDir(pid), ".json")
	if err != nil {
		return nil, err
	}
	out := readObjectsFromFiles[Replay](paths, nil)
	sort.Slice(out, func(i, j int) bool {
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out, nil
}

func (s *Store) DeleteReplay(pid, rid string) error {
	return removeFile(filepath.Join(s.replaysDir(pid), rid+".json"))
}

func (s *Store) notebooksDir(pid string) string {
	return filepath.Join(s.projectDir(pid), "notebooks")
}

func (s *Store) SaveNotebook(n Notebook) error {
	return atomicWriteJSON(filepath.Join(s.notebooksDir(n.ProjectID), n.ID+".json"), n)
}

func (s *Store) GetNotebook(pid, nid string) (Notebook, error) {
	var v Notebook
	err := readJSON(filepath.Join(s.notebooksDir(pid), nid+".json"), &v)
	return v, err
}

func (s *Store) ListNotebooks(pid string) ([]Notebook, error) {
	paths, err := listDir(s.notebooksDir(pid), ".json")
	if err != nil {
		return nil, err
	}
	out := readObjectsFromFiles[Notebook](paths, nil)
	sort.Slice(out, func(i, j int) bool {
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out, nil
}

func (s *Store) DeleteNotebook(pid, nid string) error {
	return removeFile(filepath.Join(s.notebooksDir(pid), nid+".json"))
}
