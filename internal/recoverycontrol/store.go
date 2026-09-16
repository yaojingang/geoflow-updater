// Package recoverycontrol owns restore generations outside business snapshots.
package recoverycontrol

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"syscall"
)

const MinimumProtocol uint64 = 5

var identifier = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{7,127}$`)
var hex32 = regexp.MustCompile(`^[a-f0-9]{32}$`)
var hex64 = regexp.MustCompile(`^[a-f0-9]{64}$`)
var pointID = regexp.MustCompile(`^[0-9]{8}T[0-9]{6}Z-[a-f0-9]{8}$`)

type State struct {
	SchemaVersion          int     `json:"schema_version"`
	InstanceID             string  `json:"instance_id"`
	HostID                 string  `json:"host_id"`
	Epoch                  string  `json:"epoch"`
	TransactionID          *string `json:"transaction_id"`
	Phase                  string  `json:"phase"`
	MinimumUpdaterProtocol uint64  `json:"minimum_updater_protocol"`
}

type Authority struct {
	State               State  `json:"state"`
	PointID             string `json:"point_id,omitempty"`
	AdminDigest         string `json:"admin_digest,omitempty"`
	DataRestored        bool   `json:"data_restored"`
	HTTPOpened          bool   `json:"http_opened"`
	RedisManifestSHA256 string `json:"redis_manifest_sha256,omitempty"`
	PreparationSHA256   string `json:"preparation_sha256,omitempty"`
}

type Store struct{ StateDir string }

func (s Store) PublicDir(instance string) string {
	return filepath.Join(s.StateDir, "recovery-control", instance, "public")
}

// Initialize is used when enabling the managed contract, before starting PHP.
// Once initialized, lost authority must be recovered by an operator.
func (s Store) Initialize(instance string) (Authority, error) {
	return s.change(instance, true, func(a *Authority) error { return nil })
}

func (s Store) Begin(instance, point, transaction, adminDigest string) (Authority, error) {
	if !pointID.MatchString(point) || !identifier.MatchString(transaction) || !hex64.MatchString(adminDigest) {
		return Authority{}, errors.New("invalid recovery transaction identity")
	}
	return s.change(instance, true, func(a *Authority) error {
		if a.State.TransactionID != nil && *a.State.TransactionID == transaction {
			if a.PointID != point || a.AdminDigest != adminDigest {
				return errors.New("recovery transaction identity conflict")
			}
			return nil
		}
		epoch, err := randomID()
		if err != nil {
			return err
		}
		a.State.Epoch, a.State.TransactionID, a.State.Phase = epoch, &transaction, "restoring"
		a.PointID, a.AdminDigest, a.DataRestored, a.HTTPOpened, a.RedisManifestSHA256 = point, adminDigest, false, false, ""
		a.PreparationSHA256 = ""
		return nil
	})
}

func (s Store) Restored(instance, transaction string) (Authority, error) {
	return s.change(instance, false, func(a *Authority) error {
		if a.State.TransactionID == nil || *a.State.TransactionID != transaction || a.State.Phase != "restoring" {
			return errors.New("recovery completion identity conflict")
		}
		a.DataRestored = true
		a.State.Phase = "validating"
		return nil
	})
}

// OpenHTTP retains the background hold. Only a separate verified reconciliation
// may ever introduce a transition to ready after restoration.
func (s Store) OpenHTTP(instance, transaction string) (Authority, error) {
	return s.change(instance, false, func(a *Authority) error {
		if a.State.TransactionID == nil || *a.State.TransactionID != transaction || !a.DataRestored || a.RedisManifestSHA256 == "" || a.PreparationSHA256 == "" || (a.State.Phase != "validating" && a.State.Phase != "http_ready") {
			return errors.New("recovery HTTP boundary identity conflict")
		}
		a.HTTPOpened = true
		a.State.Phase = "http_ready"
		return nil
	})
}

func (s Store) RecordPreparation(instance, transaction, digest string) (Authority, error) {
	if !hex64.MatchString(digest) {
		return Authority{}, errors.New("invalid Core preparation digest")
	}
	return s.change(instance, false, func(a *Authority) error {
		if a.State.TransactionID == nil || *a.State.TransactionID != transaction || a.State.Phase != "validating" {
			return errors.New("Core preparation transaction mismatch")
		}
		if a.PreparationSHA256 != "" && a.PreparationSHA256 != digest {
			return errors.New("Core preparation report changed")
		}
		a.PreparationSHA256 = digest
		return nil
	})
}

func (s Store) RecordRedisManifest(instance, transaction, digest string) (Authority, error) {
	if !hex64.MatchString(digest) {
		return Authority{}, errors.New("invalid Redis quarantine digest")
	}
	return s.change(instance, false, func(a *Authority) error {
		if a.State.TransactionID == nil || *a.State.TransactionID != transaction || a.State.Phase != "validating" {
			return errors.New("Redis quarantine transaction mismatch")
		}
		if a.RedisManifestSHA256 != "" && a.RedisManifestSHA256 != digest {
			return errors.New("Redis quarantine manifest changed")
		}
		a.RedisManifestSHA256 = digest
		return nil
	})
}

func (s Store) Read(instance string) (Authority, error) {
	var a Authority
	if instance != "primary" || !filepath.IsAbs(s.StateDir) {
		return a, errors.New("invalid recovery control location")
	}
	path := filepath.Join(s.StateDir, "recovery-control", instance, "authority.json")
	info, err := os.Lstat(path)
	if err != nil {
		return a, err
	}
	if !info.Mode().IsRegular() || info.Size() > 16384 || info.Mode().Perm()&0077 != 0 {
		return a, errors.New("unsafe recovery authority file")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return a, err
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err = d.Decode(&a); err != nil {
		return a, errors.New("invalid recovery authority")
	}
	var extra any
	if !errors.Is(d.Decode(&extra), io.EOF) {
		return a, errors.New("trailing recovery authority data")
	}
	if err = validate(a, instance); err != nil {
		return a, err
	}
	return a, nil
}

func validate(a Authority, instance string) error {
	st := a.State
	if st.SchemaVersion != 1 || st.InstanceID != instance || !hex32.MatchString(st.HostID) || !hex32.MatchString(st.Epoch) || st.MinimumUpdaterProtocol != MinimumProtocol {
		return errors.New("invalid recovery authority identity")
	}
	switch st.Phase {
	case "ready":
		if st.TransactionID != nil || a.PointID != "" || a.DataRestored || a.HTTPOpened {
			return errors.New("invalid initial recovery state")
		}
	case "restoring", "validating", "http_ready":
		if st.TransactionID == nil || !identifier.MatchString(*st.TransactionID) || !pointID.MatchString(a.PointID) || !hex64.MatchString(a.AdminDigest) {
			return errors.New("invalid recovery transaction state")
		}
		if (st.Phase != "restoring") != a.DataRestored || (st.Phase == "http_ready") != a.HTTPOpened {
			return errors.New("inconsistent recovery boundary")
		}
	default:
		return errors.New("unknown recovery phase")
	}
	if a.RedisManifestSHA256 != "" && !hex64.MatchString(a.RedisManifestSHA256) {
		return errors.New("invalid quarantine manifest")
	}
	if a.PreparationSHA256 != "" && !hex64.MatchString(a.PreparationSHA256) {
		return errors.New("invalid Core preparation proof")
	}
	return nil
}

func (s Store) change(instance string, initialize bool, mutate func(*Authority) error) (Authority, error) {
	var a Authority
	if instance != "primary" || !filepath.IsAbs(s.StateDir) {
		return a, errors.New("invalid recovery control location")
	}
	if err := CheckProtocol(s.StateDir, MinimumProtocol); err != nil {
		return a, err
	}
	for _, p := range []string{s.StateDir, filepath.Join(s.StateDir, "recovery-control"), filepath.Join(s.StateDir, "recovery-control", instance)} {
		if err := directory(p, 0700); err != nil {
			return a, err
		}
	}
	dir := filepath.Join(s.StateDir, "recovery-control", instance)
	lock, err := os.OpenFile(filepath.Join(dir, "control.lock"), os.O_RDWR|os.O_CREATE|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return a, err
	}
	defer lock.Close()
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return a, err
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	a, err = s.Read(instance)
	if errors.Is(err, os.ErrNotExist) && initialize {
		if _, e := os.Lstat(filepath.Join(dir, "initialized")); !errors.Is(e, os.ErrNotExist) {
			return a, errors.New("recovery authority is missing after initialization")
		}
		host, e := randomID()
		if e != nil {
			return a, e
		}
		epoch, e := randomID()
		if e != nil {
			return a, e
		}
		a.State = State{SchemaVersion: 1, InstanceID: instance, HostID: host, Epoch: epoch, Phase: "ready", MinimumUpdaterProtocol: MinimumProtocol}
		if e = atomic(filepath.Join(s.StateDir, "minimum-updater-protocol"), []byte("5\n"), 0600); e != nil {
			return a, e
		}
		if e = atomic(filepath.Join(dir, "initialized"), []byte("1\n"), 0600); e != nil {
			return a, e
		}
	} else if err != nil {
		return a, err
	}
	if err = mutate(&a); err != nil {
		return a, err
	}
	if err = validate(a, instance); err != nil {
		return a, err
	}
	if err = directory(s.PublicDir(instance), 0755); err != nil {
		return a, err
	}
	private, err := json.Marshal(a)
	if err != nil {
		return a, err
	}
	public, err := json.Marshal(a.State)
	if err != nil {
		return a, err
	}
	// Authority is committed first. No restore caller may write business data
	// unless both files and parent directories are durably published.
	if err = atomic(filepath.Join(s.StateDir, "minimum-updater-protocol"), []byte("5\n"), 0600); err != nil {
		return a, err
	}
	if err = atomic(filepath.Join(dir, "authority.json"), append(private, '\n'), 0600); err != nil {
		return a, err
	}
	err = atomic(filepath.Join(s.PublicDir(instance), "state.json"), append(public, '\n'), 0644)
	return a, err
}

func randomID() (string, error) {
	var b [16]byte
	_, err := rand.Read(b[:])
	return hex.EncodeToString(b[:]), err
}
func directory(path string, mode os.FileMode) error {
	if err := os.Mkdir(path, mode); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("unsafe recovery directory %s", path)
	}
	if mode == 0755 {
		// The daemon uses umask 0027; container UID 33 must traverse the public
		// bind root even when it has no membership in the host control group.
		if err := os.Chmod(path, mode); err != nil {
			return err
		}
	}
	return syncDir(filepath.Dir(path))
}
func atomic(path string, data []byte, mode os.FileMode) error {
	if info, err := os.Lstat(path); err == nil && !info.Mode().IsRegular() {
		return errors.New("unsafe recovery state destination")
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".recovery-state-")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if err = f.Chmod(mode); err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	err = errors.Join(err, f.Close())
	if err != nil {
		return err
	}
	if err = os.Rename(name, path); err != nil {
		return err
	}
	return syncDir(filepath.Dir(path))
}
func syncDir(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}
