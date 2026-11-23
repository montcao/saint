package sandbox

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/montcao/saint/internal/toolprofile"
)

type profileMount struct {
	Path        string `json:"path"`
	Destination string `json:"destination"`
	ReadOnly    bool   `json:"read_only"`
}

type profileRecord struct {
	Name        string               `json:"name"`
	Owner       string               `json:"owner"`
	Directory   string               `json:"directory"`
	Manifest    toolprofile.Manifest `json:"manifest"`
	Profile     string               `json:"profile"`
	Runtime     Runtime              `json:"runtime"`
	Script      string               `json:"script"`
	Environment []string             `json:"environment"`
	Mounts      []profileMount       `json:"mounts"`
	Network     string               `json:"network"`
	Workdir     string               `json:"workdir"`
	CPUs        int                  `json:"cpus"`
	MemoryGB    int                  `json:"memory_gb"`
}

func (a *AppleContainer) SetProfileRoot(root string) { a.profileRoot = root }

func (a *AppleContainer) PreflightProfile(ctx context.Context, ref string) error {
	if a.profileRoot == "" {
		return errors.New("experimental profile state is not configured")
	}
	if err := a.checkProfileCLI(ctx); err != nil {
		return err
	}
	_, err := toolprofile.Preflight(ctx, ref)
	return err
}

func (a *AppleContainer) profilePath(name string) string {
	return filepath.Join(a.profileRoot, name, "owner.json")
}

func (a *AppleContainer) profileRecord(name string) (*profileRecord, error) {
	if a.profileRoot == "" {
		return nil, nil
	}
	if err := ValidateContainerName(name); err != nil {
		return nil, err
	}
	p := a.profilePath(name)
	if _, err := os.Lstat(p); os.IsNotExist(err) {
		return nil, nil
	}
	if err := toolprofile.PrivatePath(p, false); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	var r profileRecord
	if err = json.Unmarshal(data, &r); err != nil {
		return nil, err
	}
	if r.Name != name || r.Directory != filepath.Dir(p) || len(r.Owner) != 64 {
		return nil, errors.New("experimental ownership record mismatch")
	}
	return &r, nil
}

// RequireProfile prevents a missing ledger from falling through to legacy rm/start.
func (a *AppleContainer) RequireProfile(name, ref string) error {
	r, err := a.profileRecord(name)
	if err != nil {
		return err
	}
	if r == nil || r.Profile != ref {
		return errors.New("experimental ownership record is missing or differs; refusing legacy fallback")
	}
	return nil
}

func (a *AppleContainer) profileOperation(ctx context.Context, name, op string) (bool, error) {
	r, err := a.profileRecord(name)
	if err != nil || r == nil {
		return false, err
	}
	if err := toolprofile.Call(ctx, r.Manifest.Adapter, op, r); err != nil {
		return true, err
	}
	if op == "remove" {
		// Keep ownership, but erase launch-time session tokens and commands.
		r.Environment = nil
		r.Script = ""
		r.Mounts = nil
		return true, a.saveProfileRecord(r)
	}
	return true, nil
}

func (a *AppleContainer) createProfile(ctx context.Context, name string, rt Runtime, repos []Repo, sessionDir string, opts CreateOptions, args []string, script string) error {
	if opts.AuthMode == AuthSubscription {
		return errors.New("experimental profile does not accept subscription credentials")
	}
	m, err := toolprofile.Preflight(ctx, opts.ExperimentalProfile)
	if err != nil {
		return err
	}
	if err = os.MkdirAll(a.profileRoot, 0o700); err != nil {
		return err
	}
	if err = toolprofile.PrivatePath(a.profileRoot, true); err != nil {
		return err
	}
	dir := filepath.Join(a.profileRoot, name)
	r := profileRecord{Name: name, Directory: dir, Manifest: m, Profile: opts.ExperimentalProfile, Runtime: rt, Script: script, Network: opts.Network, Workdir: repos[0].Path, CPUs: opts.CPUs, MemoryGB: opts.MemoryGB}
	if r.CPUs <= 0 {
		r.CPUs = 2
	}
	if r.MemoryGB <= 0 {
		r.MemoryGB = 2
	}
	// Reuse the existing bootstrap and its explicitly constructed proxy environment.
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "--env" {
			r.Environment = append(r.Environment, args[i+1])
			i++
		}
	}
	for _, repo := range repos {
		r.Mounts = append(r.Mounts, profileMount{repo.Path, repo.Path, repo.ReadOnly})
	}
	for _, skill := range opts.SkillMounts {
		r.Mounts = append(r.Mounts, profileMount{skill.Path, "/saint/skills/" + skill.Name, true})
	}
	r.Mounts = append(r.Mounts, profileMount{sessionDir, "/saint", false})
	manifestPath, _, _ := toolprofile.ParseRef(opts.ExperimentalProfile)
	for _, mount := range r.Mounts {
		for _, protected := range []string{a.profileRoot, manifestPath, m.Adapter.Path, m.Tools.Path, m.Root.Path, m.Kernel.Path, m.Review.Path} {
			if toolprofile.Overlaps(mount.Path, protected) {
				return fmt.Errorf("guest mount overlaps protected profile state/artifact: %s", mount.Path)
			}
		}
	}
	reserved, err := a.profileRecord(name)
	if err != nil || reserved == nil {
		return errors.New("missing exclusive profile reservation")
	}
	if reserved.Profile != opts.ExperimentalProfile {
		return errors.New("profile reservation differs")
	}
	r.Owner = reserved.Owner
	if err = a.saveProfileRecord(&r); err != nil {
		return err
	}
	// The pending record survives failure, timeout or process death. The helper
	// must prove VM absence and mount ownership before releasing any disk.
	if err = toolprofile.Call(ctx, m.Adapter, "create", r); err != nil {
		return err
	}
	return nil
}

// ReserveProfile creates durable identity before any network or VM mutation.
func (a *AppleContainer) ReserveProfile(ctx context.Context, name, ref string) error {
	if err := ValidateContainerName(name); err != nil {
		return err
	}
	m, err := toolprofile.Preflight(ctx, ref)
	if err != nil {
		return err
	}
	stdout, _, err := a.runProfile(ctx, "ls", "--all", "--format", "json")
	if err != nil {
		return err
	}
	inventory, err := ParseListJSON(stdout)
	if err != nil {
		return err
	}
	for _, vm := range inventory {
		if vm.Name == name {
			return errors.New("container name already exists")
		}
	}
	data, _, err := a.runProfile(ctx, "network", "ls", "--format", "json")
	if err != nil {
		return err
	}
	exists, err := networkExists(data, name)
	if err != nil {
		return err
	}
	if exists {
		return errors.New("network name already exists")
	}
	if err = os.MkdirAll(a.profileRoot, 0o700); err != nil {
		return err
	}
	if err = toolprofile.PrivatePath(a.profileRoot, true); err != nil {
		return err
	}
	dir := filepath.Join(a.profileRoot, name)
	if err = os.Mkdir(dir, 0o700); err != nil {
		return err
	}
	var nonce [32]byte
	if _, err = rand.Read(nonce[:]); err != nil {
		return err
	}
	record := profileRecord{Name: name, Owner: hex.EncodeToString(nonce[:]), Directory: dir, Manifest: m, Profile: ref}
	data, err = json.Marshal(record)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(a.profilePath(name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, werr := f.Write(data)
	serr := f.Sync()
	cerr := f.Close()
	if err = errors.Join(werr, serr, cerr); err != nil {
		return err
	}
	return errors.Join(syncProfileDirectory(dir), syncProfileDirectory(a.profileRoot))
}

func syncProfileDirectory(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}
func (a *AppleContainer) saveProfileRecord(r *profileRecord) error {
	data, err := json.Marshal(r)
	if err != nil {
		return err
	}
	temp, err := os.CreateTemp(r.Directory, "owner-next-")
	if err != nil {
		return err
	}
	defer os.Remove(temp.Name())
	_, werr := temp.Write(data)
	serr := temp.Sync()
	cerr := temp.Close()
	if err = errors.Join(werr, serr, cerr); err != nil {
		return err
	}
	if err = os.Rename(temp.Name(), a.profilePath(r.Name)); err != nil {
		return err
	}
	return syncProfileDirectory(r.Directory)
}
