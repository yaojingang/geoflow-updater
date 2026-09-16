package coordination

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

// Head preserves ordering across root/v1 starts and request-based admissions.
// For legacy starts its snapshot is authoritative. For admissions the receipt
// remains authoritative and this head is a rebuildable projection.
type Head struct {
	SchemaVersion int             `json:"schema_version"`
	InstanceID    string          `json:"instance_id"`
	Sequence      uint64          `json:"sequence"`
	Origin        string          `json:"origin"`
	OperationID   string          `json:"operation_id"`
	Operation     json.RawMessage `json:"operation"`
}

func (h Head) valid() bool {
	return h.SchemaVersion == 2 && h.InstanceID == "primary" && h.Sequence > 0 && (h.Origin == "legacy" || h.Origin == "admission") && opID.MatchString(h.OperationID) && json.Valid(h.Operation)
}
func (s Store) head(id string) (Head, error) {
	var h Head
	root, err := s.root(id, "")
	if err != nil {
		return h, err
	}
	if err = read(filepath.Join(root, "head.json"), &h); err != nil {
		return h, err
	}
	if !h.valid() || h.InstanceID != id {
		return h, errors.New("invalid operation order authority")
	}
	return h, nil
}
func (s Store) writeHead(h Head) error {
	if !h.valid() {
		return errors.New("invalid operation order authority")
	}
	root, err := s.root(h.InstanceID, "")
	if err != nil {
		return err
	}
	return s.write(filepath.Join(root, "head.json"), h, false)
}

// Latest selects an operation by durable order, including explicit host/v1
// recovery that supersedes an earlier unresolved v2 request.
func (s Store) Latest(id string) (Head, error) {
	if id != "primary" {
		return Head{}, os.ErrNotExist
	}
	records, err := s.Admissions(id)
	if err != nil {
		return Head{}, err
	}
	head, err := s.head(id)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return Head{}, err
	}
	var latest Head
	if err == nil {
		if head.Origin == "legacy" {
			latest = head
		} else {
			found := false
			for _, record := range records {
				if record.OperationID == head.OperationID && record.Sequence == head.Sequence {
					found = true
					break
				}
			}
			if !found {
				return Head{}, errors.New("operation head lost its admission authority")
			}
		}
	}
	for _, record := range records {
		if latest.Sequence == record.Sequence && latest.OperationID != record.OperationID {
			return Head{}, errors.New("conflicting operation order authority")
		}
		if record.Sequence > latest.Sequence {
			latest = Head{2, id, record.Sequence, "admission", record.OperationID, record.Operation}
		}
	}
	if latest.Sequence == 0 {
		return Head{}, os.ErrNotExist
	}
	return latest, nil
}
func (s Store) nextSequence(id string) (uint64, error) {
	latest, err := s.Latest(id)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return 0, err
	}
	if latest.Sequence == ^uint64(0) {
		return 0, errors.New("operation sequence exhausted")
	}
	return latest.Sequence + 1, nil
}
func (s Store) BeginLegacy(id, operation string, contents json.RawMessage) error {
	if id != "primary" {
		return nil
	}
	sequence, err := s.nextSequence(id)
	if err != nil {
		return err
	}
	return s.writeHead(Head{2, id, sequence, "legacy", operation, contents})
}
func (s Store) syncHeadOperation(id, operation string, contents json.RawMessage) error {
	latest, err := s.Latest(id)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if latest.OperationID != operation {
		return errors.New("operation was superseded by a newer operation")
	}
	latest.Operation = contents
	return s.writeHead(latest)
}

// SyncAuthority closes any uncertain ancestor-directory sync before recovery
// can expose the authority through writable operation projections.
func (s Store) SyncAuthority(id string) error {
	root, err := s.root(id, "")
	if err != nil {
		return err
	}
	if err = s.durableDirectory(root); err != nil {
		return err
	}
	records, err := s.Admissions(id)
	if err != nil {
		return err
	}
	if len(records) > 0 {
		return s.durableDirectory(filepath.Join(root, "admissions"))
	}
	return nil
}
