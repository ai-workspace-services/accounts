package migrate

import (
	"errors"
	"io"
	"os"
	"testing"
)

func TestReviewedSourceExposesOnlyNextSQLAndCurrentMetadata(t *testing.T) {
	s := &reviewedSource{expected: 2026100601, target: 2026100701, name: "billing.up.sql", body: []byte("CREATE TABLE fixture(id int);")}
	first, err := s.First()
	if err != nil || first != s.target {
		t.Fatal("checkpoint must never be the first executable migration")
	}
	next, err := s.Next(s.expected)
	if err != nil || next != s.target {
		t.Fatal("reviewed next migration missing")
	}
	if _, err = s.Next(s.target); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("later migration exposed")
	}
	if _, err = s.Prev(s.target); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("downgrade path exposed")
	}
	for _, version := range []uint{s.expected, s.target} {
		reader, _, err := s.ReadUp(version)
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(reader)
		reader.Close()
		if err != nil {
			t.Fatal(err)
		}
		if version == s.expected && len(body) != 0 {
			t.Fatal("current checkpoint contains historical SQL")
		}
		if version == s.target && string(body) != string(s.body) {
			t.Fatal("reviewed SQL bytes differ")
		}
		if _, _, err = s.ReadDown(version); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("down SQL exposed")
		}
	}
	if _, _, err = s.ReadUp(1); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("unexpected SQL exposed")
	}
	if _, err = s.Open("file://arbitrary"); err == nil {
		t.Fatal("URL source override accepted")
	}
}
