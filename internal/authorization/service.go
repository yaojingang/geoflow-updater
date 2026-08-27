package authorization

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/yaojingang/geoflow-updater/internal/instance"
	"gopkg.in/yaml.v3"
)

var (
	ErrUnconfigured = errors.New("mutation authorization is not configured")
	ErrInvalid      = errors.New("mutation authorization code is invalid")
	ErrReplay       = errors.New("mutation authorization code was already used")
	ErrRateLimited  = errors.New("mutation authorization attempts are rate limited")

	instanceIDPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)
	codePattern       = regexp.MustCompile(`^[0-9]{6}$`)
	secretPattern     = regexp.MustCompile(`^[A-Z2-7]{32}$`)
)

const (
	issuer             = "GEOFlow Updater"
	period             = int64(30)
	lockoutThreshold   = 5
	baseLockoutSeconds = int64(15 * 60)
	maxLockoutSeconds  = int64(24 * 60 * 60)
)

type Scope string

const (
	ScopeUpdate   Scope = "update"
	ScopeBackup   Scope = "backup"
	ScopeRollback Scope = "rollback"
)

var scopes = []Scope{ScopeUpdate, ScopeBackup, ScopeRollback}

type Factor struct {
	Scope Scope
	URI   string
}

type Provisioning struct {
	Factors []Factor
}

type attemptState struct {
	Failures    int
	LockedUntil int64
}

type Service struct {
	StateDir string
	Now      func() time.Time
	Random   io.Reader
}

func (service Service) Provision(instanceID string) (Provisioning, error) {
	instanceDir, err := service.instanceDir(instanceID)
	if err != nil {
		return Provisioning{}, err
	}
	if err := validateEnrollment(instanceDir, instanceID); err != nil {
		return Provisioning{}, fmt.Errorf("validate managed enrollment: %w", err)
	}
	secretPath := filepath.Join(instanceDir, "mutation.secret")
	secret, err := readSecret(secretPath)
	if errors.Is(err, os.ErrNotExist) {
		secret, err = service.createSecret(secretPath)
	}
	if err != nil {
		return Provisioning{}, err
	}

	factors := make([]Factor, 0, len(scopes))
	for _, scope := range scopes {
		scopedSecret, err := deriveSecret(secret, scope)
		if err != nil {
			return Provisioning{}, err
		}
		factors = append(factors, Factor{
			Scope: scope,
			URI:   provisioningURI(instanceID, scope, scopedSecret),
		})
	}

	return Provisioning{Factors: factors}, nil
}

func (service Service) Configured(instanceID string) error {
	instanceDir, err := service.instanceDir(instanceID)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ErrUnconfigured
		}
		return err
	}
	if err := validateEnrollment(instanceDir, instanceID); err != nil {
		return ErrUnconfigured
	}
	_, err = readSecret(filepath.Join(instanceDir, "mutation.secret"))
	if errors.Is(err, os.ErrNotExist) {
		return ErrUnconfigured
	}

	return err
}

func (service Service) Authorize(instanceID string, scope Scope, code string, callback func() error) error {
	if !validScope(scope) || !codePattern.MatchString(code) || callback == nil {
		return ErrInvalid
	}
	instanceDir, err := service.instanceDir(instanceID)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ErrUnconfigured
		}
		return err
	}
	lock, err := openPrivateLock(filepath.Join(instanceDir, "mutation.lock"))
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("lock mutation authorization: %w", err)
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)

	masterSecret, err := readSecret(filepath.Join(instanceDir, "mutation.secret"))
	if errors.Is(err, os.ErrNotExist) {
		return ErrUnconfigured
	}
	if err != nil {
		return err
	}
	scopedSecret, err := deriveSecret(masterSecret, scope)
	if err != nil {
		return err
	}
	attemptsPath := filepath.Join(instanceDir, "mutation.attempts")
	attempts, err := readAttemptState(attemptsPath)
	if err != nil {
		return err
	}
	now := service.now()
	if attempts.LockedUntil > now.Unix() {
		return ErrRateLimited
	}
	counterPath := filepath.Join(instanceDir, "mutation."+string(scope)+".counter")
	lastCounter, err := readCounter(counterPath)
	if err != nil {
		return err
	}
	currentCounter := now.Unix() / period
	matchedCounter := int64(-1)
	for _, candidate := range []int64{currentCounter - 1, currentCounter, currentCounter + 1} {
		expected, err := generateCode(scopedSecret, candidate)
		if err != nil {
			return err
		}
		if subtle.ConstantTimeCompare([]byte(expected), []byte(code)) == 1 && candidate > matchedCounter {
			matchedCounter = candidate
		}
	}
	if matchedCounter < 0 {
		return recordInvalidAttempt(attemptsPath, attempts, now)
	}
	if matchedCounter <= lastCounter {
		return ErrReplay
	}
	if err := replacePrivateFile(attemptsPath, []byte("0 0\n")); err != nil {
		return fmt.Errorf("reset mutation authorization attempts: %w", err)
	}
	if err := replacePrivateFile(counterPath, []byte(strconv.FormatInt(matchedCounter, 10)+"\n")); err != nil {
		return fmt.Errorf("persist mutation authorization counter: %w", err)
	}
	if err := callback(); err != nil {
		if restoreErr := restoreCounter(counterPath, lastCounter); restoreErr != nil {
			return errors.Join(err, fmt.Errorf("restore unused mutation authorization counter: %w", restoreErr))
		}

		return err
	}

	return nil
}

func (service Service) instanceDir(instanceID string) (string, error) {
	if !instanceIDPattern.MatchString(instanceID) {
		return "", errors.New("managed instance identifier is invalid")
	}
	stateDir := service.StateDir
	if stateDir == "" {
		stateDir = "/var/lib/geoflow-updater"
	}
	instanceDir := filepath.Join(stateDir, "instances", instanceID)
	info, err := os.Lstat(instanceDir)
	if err != nil {
		return "", err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("managed instance directory is unavailable or symbolic")
	}

	return instanceDir, nil
}

func validateEnrollment(instanceDir string, instanceID string) error {
	path := filepath.Join(instanceDir, "instance.yml")
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o027 != 0 || info.Size() < 1 || info.Size() > 1024*1024 {
		return errors.New("managed instance configuration is not a private bounded regular file")
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var config instance.Config
	decoder := yaml.NewDecoder(bytes.NewReader(contents))
	decoder.KnownFields(true)
	if err := decoder.Decode(&config); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("managed instance configuration contains multiple documents")
	}
	if config.SchemaVersion != 1 || config.ID != instanceID || !filepath.IsAbs(config.Root) ||
		!filepath.IsAbs(config.ComposeFile) || !filepath.IsAbs(config.EnvironmentFile) || !filepath.IsAbs(config.ControlToken) ||
		config.ReleaseSequence < 1 || strings.TrimSpace(config.Version) == "" || config.EnrolledAt.IsZero() {
		return errors.New("managed instance configuration is incomplete")
	}

	return nil
}

func (service Service) createSecret(path string) (string, error) {
	random := service.Random
	if random == nil {
		random = rand.Reader
	}
	secretBytes := make([]byte, 20)
	if _, err := io.ReadFull(random, secretBytes); err != nil {
		return "", fmt.Errorf("generate mutation authorization secret: %w", err)
	}
	secret := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(secretBytes)
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, os.ErrExist) {
		return readSecret(path)
	}
	if err != nil {
		return "", fmt.Errorf("create mutation authorization secret: %w", err)
	}
	remove := true
	defer func() {
		_ = file.Close()
		if remove {
			_ = os.Remove(path)
		}
	}()
	if err := file.Chmod(0o600); err != nil {
		return "", err
	}
	if _, err := file.WriteString(secret + "\n"); err != nil {
		return "", err
	}
	if err := file.Sync(); err != nil {
		return "", err
	}
	if err := file.Close(); err != nil {
		return "", err
	}
	remove = false
	if err := syncDirectory(filepath.Dir(path)); err != nil {
		return "", err
	}

	return secret, nil
}

func (service Service) now() time.Time {
	if service.Now != nil {
		return service.Now()
	}

	return time.Now()
}

func validScope(scope Scope) bool {
	return scope == ScopeUpdate || scope == ScopeBackup || scope == ScopeRollback
}

func deriveSecret(encodedMaster string, scope Scope) (string, error) {
	if !validScope(scope) {
		return "", ErrInvalid
	}
	master, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(encodedMaster)
	if err != nil {
		return "", err
	}
	digest := hmac.New(sha256.New, master)
	_, _ = digest.Write([]byte("geoflow-updater-mutation:" + string(scope)))

	return base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(digest.Sum(nil)[:20]), nil
}

func readSecret(path string) (string, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || info.Size() < 1 || info.Size() > 128 {
		return "", errors.New("mutation authorization secret is not a private bounded regular file")
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	secret := strings.TrimSpace(string(contents))
	if !secretPattern.MatchString(secret) {
		return "", errors.New("mutation authorization secret format is invalid")
	}

	return secret, nil
}

func readCounter(path string) (int64, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return -1, nil
	}
	if err != nil {
		return 0, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || info.Size() < 1 || info.Size() > 32 {
		return 0, errors.New("mutation authorization counter is invalid")
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	counter, err := strconv.ParseInt(strings.TrimSpace(string(contents)), 10, 64)
	if err != nil || counter < 0 {
		return 0, errors.New("mutation authorization counter is invalid")
	}

	return counter, nil
}

func readAttemptState(path string) (attemptState, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return attemptState{}, nil
	}
	if err != nil {
		return attemptState{}, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || info.Size() < 1 || info.Size() > 64 {
		return attemptState{}, errors.New("mutation authorization attempt state is invalid")
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		return attemptState{}, err
	}
	fields := strings.Fields(string(contents))
	if len(fields) != 2 {
		return attemptState{}, errors.New("mutation authorization attempt state is invalid")
	}
	failures, failuresErr := strconv.Atoi(fields[0])
	lockedUntil, lockErr := strconv.ParseInt(fields[1], 10, 64)
	if failuresErr != nil || lockErr != nil || failures < 0 || failures > 1_000_000 || lockedUntil < 0 || (failures < lockoutThreshold && lockedUntil != 0) {
		return attemptState{}, errors.New("mutation authorization attempt state is invalid")
	}

	return attemptState{Failures: failures, LockedUntil: lockedUntil}, nil
}

func recordInvalidAttempt(path string, previous attemptState, now time.Time) error {
	failures := previous.Failures + 1
	lockedUntil := int64(0)
	result := ErrInvalid
	if failures >= lockoutThreshold {
		exponent := failures - lockoutThreshold
		if exponent > 16 {
			exponent = 16
		}
		delay := baseLockoutSeconds << exponent
		if delay > maxLockoutSeconds {
			delay = maxLockoutSeconds
		}
		lockedUntil = now.Unix() + delay
		result = ErrRateLimited
	}
	if err := replacePrivateFile(path, []byte(fmt.Sprintf("%d %d\n", failures, lockedUntil))); err != nil {
		return fmt.Errorf("persist mutation authorization attempts: %w", err)
	}

	return result
}

func restoreCounter(path string, previous int64) error {
	if previous >= 0 {
		return replacePrivateFile(path, []byte(strconv.FormatInt(previous, 10)+"\n"))
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}

	return syncDirectory(filepath.Dir(path))
}

func openPrivateLock(path string) (*os.File, error) {
	if info, err := os.Lstat(path); err == nil && (!info.Mode().IsRegular() || info.Mode().Perm() != 0o600) {
		return nil, errors.New("mutation authorization lock is invalid")
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	lock, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open mutation authorization lock: %w", err)
	}
	if err := lock.Chmod(0o600); err != nil {
		_ = lock.Close()
		return nil, err
	}

	return lock, nil
}

func replacePrivateFile(path string, contents []byte) error {
	directory := filepath.Dir(path)
	temporary, err := os.CreateTemp(directory, ".mutation-state-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer func() {
		_ = temporary.Close()
		_ = os.Remove(temporaryPath)
	}()
	if err := temporary.Chmod(0o600); err != nil {
		return err
	}
	if _, err := temporary.Write(contents); err != nil {
		return err
	}
	if err := temporary.Sync(); err != nil {
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return err
	}

	return syncDirectory(directory)
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()

	return directory.Sync()
}

func provisioningURI(instanceID string, scope Scope, secret string) string {
	query := url.Values{
		"algorithm": {"SHA1"},
		"digits":    {"6"},
		"issuer":    {issuer},
		"period":    {strconv.FormatInt(period, 10)},
		"secret":    {secret},
	}
	uri := url.URL{
		Scheme:   "otpauth",
		Host:     "totp",
		Path:     "/" + issuer + ":" + instanceID + ":" + string(scope),
		RawQuery: query.Encode(),
	}

	return uri.String()
}

func generateCode(encodedSecret string, counter int64) (string, error) {
	secret, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(encodedSecret)
	if err != nil {
		return "", err
	}
	message := make([]byte, 8)
	binary.BigEndian.PutUint64(message, uint64(counter))
	digest := hmac.New(sha1.New, secret)
	_, _ = digest.Write(message)
	sum := digest.Sum(nil)
	offset := sum[len(sum)-1] & 0x0f
	value := (uint32(sum[offset])&0x7f)<<24 |
		uint32(sum[offset+1])<<16 |
		uint32(sum[offset+2])<<8 |
		uint32(sum[offset+3])

	return fmt.Sprintf("%06d", value%1_000_000), nil
}
