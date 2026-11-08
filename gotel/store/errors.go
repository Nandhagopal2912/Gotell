package store

import (
	"errors"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// ErrorKind says whether retrying a failed write can help.
type ErrorKind int

const (
	// Transient: busy, locked, disk full, I/O error. Retry with backoff.
	Transient ErrorKind = iota
	// BadRecord: a record breaks a schema rule (a constraint). Retrying
	// the same data never helps; find the record and quarantine it.
	BadRecord
	// Fatal: read-only, corrupt, not a database. Nothing can be saved.
	Fatal
)

func (k ErrorKind) String() string {
	switch k {
	case Transient:
		return "transient"
	case BadRecord:
		return "bad record"
	default:
		return "fatal"
	}
}

// kinded lets an error say its own kind. Tests use it to inject failures.
type kinded interface {
	StorageKind() ErrorKind
}

// ClassifyError decides how the writer should react to a failed write.
// Errors it does not recognise are treated as transient: retrying is
// safe (nothing is lost), and every retry is logged loudly.
func ClassifyError(err error) ErrorKind {
	var k kinded
	if errors.As(err, &k) {
		return k.StorageKind()
	}

	var se *sqlite.Error
	if !errors.As(err, &se) {
		return Transient
	}
	// Extended codes (e.g. SQLITE_IOERR_WRITE) keep the primary code in
	// the low 8 bits.
	switch se.Code() & 0xff {
	case sqlite3.SQLITE_CONSTRAINT:
		return BadRecord
	case sqlite3.SQLITE_READONLY, sqlite3.SQLITE_CORRUPT, sqlite3.SQLITE_NOTADB,
		sqlite3.SQLITE_PERM, sqlite3.SQLITE_AUTH:
		return Fatal
	default: // SQLITE_BUSY, SQLITE_LOCKED, SQLITE_FULL, SQLITE_IOERR, ...
		return Transient
	}
}
