package migrate

import (
	"bytes"
	"errors"
	"io"
	"os"

	"github.com/golang-migrate/migrate/v4/source"
)

// reviewedSource exposes one checksum-qualified forward SQL body. The expected
// database version is a metadata checkpoint only: native init already applied
// that schema. First/Next never select checkpoint SQL, and no down body exists.
// This avoids requiring historical files for directly initialized databases.
type reviewedSource struct {
	expected uint
	target   uint
	name     string
	body     []byte
}

func (s *reviewedSource) Open(string) (source.Driver, error) {
	return nil, errors.New("reviewed migration source cannot be opened by URL")
}
func (s *reviewedSource) Close() error            { return nil }
func (s *reviewedSource) First() (uint, error)    { return s.target, nil }
func (s *reviewedSource) Prev(uint) (uint, error) { return 0, os.ErrNotExist }
func (s *reviewedSource) Next(version uint) (uint, error) {
	if version == s.expected {
		return s.target, nil
	}
	return 0, os.ErrNotExist
}
func (s *reviewedSource) ReadUp(version uint) (io.ReadCloser, string, error) {
	switch version {
	case s.expected:
		// golang-migrate versionExists probes the current checkpoint. It must
		// never be selected for execution; only Next(expected)=target is exposed.
		return io.NopCloser(bytes.NewReader(nil)), "already-applied-checkpoint", nil
	case s.target:
		return io.NopCloser(bytes.NewReader(s.body)), s.name, nil
	default:
		return nil, "", os.ErrNotExist
	}
}
func (s *reviewedSource) ReadDown(uint) (io.ReadCloser, string, error) {
	return nil, "", os.ErrNotExist
}
