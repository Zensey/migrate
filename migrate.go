package migrate

import (
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/mattes/migrate/database"
	"github.com/mattes/migrate/source"
)

var DefaultPrefetchMigrations = uint(10)

var (
	ErrNoChange   = fmt.Errorf("no change")
	ErrNilVersion = fmt.Errorf("no migration")
	ErrLocked     = fmt.Errorf("database locked")
)

type ErrShortLimit struct {
	Short uint
}

func (e ErrShortLimit) Error() string {
	return fmt.Sprintf("limit %v short", e.Short)
}

type Migrate struct {
	sourceName   string
	sourceDrv    source.Driver
	databaseName string
	databaseDrv  database.Driver

	Log Logger

	GracefulStop   chan bool
	isGracefulStop bool

	isLockedMu *sync.Mutex
	isLocked   bool

	PrefetchMigrations uint
}

func New(sourceUrl, databaseUrl string) (*Migrate, error) {
	m := newCommon()

	sourceName, err := nameFromUrl(sourceUrl)
	if err != nil {
		return nil, err
	}
	m.sourceName = sourceName

	databaseName, err := nameFromUrl(databaseUrl)
	if err != nil {
		return nil, err
	}
	m.databaseName = databaseName

	sourceDrv, err := source.Open(sourceUrl)
	if err != nil {
		return nil, err
	}
	m.sourceDrv = sourceDrv

	databaseDrv, err := database.Open(databaseUrl)
	if err != nil {
		return nil, err
	}
	m.databaseDrv = databaseDrv

	return m, nil
}

func NewWithDatabaseInstance(sourceUrl string, databaseName string, databaseInstance database.Driver) (*Migrate, error) {
	m := newCommon()

	sourceName, err := nameFromUrl(sourceUrl)
	if err != nil {
		return nil, err
	}
	m.sourceName = sourceName

	m.databaseName = databaseName

	sourceDrv, err := source.Open(sourceUrl)
	if err != nil {
		return nil, err
	}
	m.sourceDrv = sourceDrv

	m.databaseDrv = databaseInstance

	return m, nil
}

func NewWithSourceInstance(sourceName string, sourceInstance source.Driver, databaseUrl string) (*Migrate, error) {
	m := newCommon()

	databaseName, err := nameFromUrl(databaseUrl)
	if err != nil {
		return nil, err
	}
	m.databaseName = databaseName

	m.sourceName = sourceName

	databaseDrv, err := database.Open(databaseUrl)
	if err != nil {
		return nil, err
	}
	m.databaseDrv = databaseDrv

	m.sourceDrv = sourceInstance

	return m, nil
}

func NewWithInstance(sourceName string, sourceInstance source.Driver, databaseName string, databaseInstance database.Driver) (*Migrate, error) {
	m := newCommon()

	m.sourceName = sourceName
	m.databaseName = databaseName

	m.sourceDrv = sourceInstance
	m.databaseDrv = databaseInstance

	return m, nil
}

func newCommon() *Migrate {
	return &Migrate{
		GracefulStop:       make(chan bool, 1),
		PrefetchMigrations: DefaultPrefetchMigrations,
		isLockedMu:         &sync.Mutex{},
	}
}

func (m *Migrate) Close() (sourceErr error, databaseErr error) {
	databaseSrvClose := make(chan error)
	sourceSrvClose := make(chan error)

	go func() {
		databaseSrvClose <- m.databaseDrv.Close()
	}()

	go func() {
		sourceSrvClose <- m.sourceDrv.Close()
	}()

	return <-sourceSrvClose, <-databaseSrvClose
}

func (m *Migrate) Migrate(version uint) error {
	if err := m.lock(); err != nil {
		return err
	}

	curVersion, err := m.databaseDrv.Version()
	if err != nil {
		return m.unlockErr(err)
	}

	ret := make(chan interface{}, m.PrefetchMigrations)
	go m.read(curVersion, int(version), ret)

	return m.unlockErr(m.runMigrations(ret))
}

func (m *Migrate) Steps(n int) error {
	if n == 0 {
		return ErrNoChange
	}

	if err := m.lock(); err != nil {
		return err
	}

	curVersion, err := m.databaseDrv.Version()
	if err != nil {
		return m.unlockErr(err)
	}

	ret := make(chan interface{}, m.PrefetchMigrations)

	if n > 0 {
		go m.readUp(curVersion, n, ret)
	} else {
		go m.readDown(curVersion, -n, ret)
	}

	return m.unlockErr(m.runMigrations(ret))
}

func (m *Migrate) Up() error {
	if err := m.lock(); err != nil {
		return err
	}

	curVersion, err := m.databaseDrv.Version()
	if err != nil {
		return m.unlockErr(err)
	}

	ret := make(chan interface{}, m.PrefetchMigrations)

	go m.readUp(curVersion, -1, ret)
	return m.unlockErr(m.runMigrations(ret))
}

func (m *Migrate) Down() error {
	if err := m.lock(); err != nil {
		return err
	}

	curVersion, err := m.databaseDrv.Version()
	if err != nil {
		return m.unlockErr(err)
	}

	ret := make(chan interface{}, m.PrefetchMigrations)
	go m.readDown(curVersion, -1, ret)
	return m.unlockErr(m.runMigrations(ret))
}

func (m *Migrate) Drop() error {
	if err := m.lock(); err != nil {
		return err
	}
	if err := m.databaseDrv.Drop(); err != nil {
		return m.unlockErr(err)
	}
	return m.unlock()
}

func (m *Migrate) Version() (uint, error) {
	v, err := m.databaseDrv.Version()
	if err != nil {
		return 0, err
	}

	if v == database.NilVersion {
		return 0, ErrNilVersion
	}

	return suint(v), nil
}

func (m *Migrate) read(from int, to int, ret chan<- interface{}) {
	defer close(ret)

	// check if from version exists
	if from >= 0 {
		if m.versionExists(suint(from)) != nil {
			ret <- os.ErrNotExist
			return
		}
	}

	// check if to version exists
	if to >= 0 {
		if m.versionExists(suint(to)) != nil {
			ret <- os.ErrNotExist
			return
		}
	}

	// no change?
	if from == to {
		ret <- ErrNoChange
		return
	}

	if from < to {
		// it's going up
		// apply first migration if from is nil version
		if from == -1 {
			firstVersion, err := m.sourceDrv.First()
			if err != nil {
				ret <- err
				return
			}

			migr, err := m.newMigration(firstVersion, int(firstVersion))
			if err != nil {
				ret <- err
				return
			}

			ret <- migr
			go migr.Buffer()
			from = int(firstVersion)
		}

		// run until we reach target ...
		for from < to {
			if m.stop() {
				return
			}

			next, err := m.sourceDrv.Next(suint(from))
			if err != nil {
				ret <- err
				return
			}

			migr, err := m.newMigration(next, int(next))
			if err != nil {
				ret <- err
				return
			}

			ret <- migr
			go migr.Buffer()
			from = int(next)
		}

	} else {
		// it's going down
		// run until we reach target ...
		for from > to && from >= 0 {
			if m.stop() {
				return
			}

			prev, err := m.sourceDrv.Prev(suint(from))
			if os.IsNotExist(err) && to == -1 {
				// apply nil migration
				migr, err := m.newMigration(suint(from), -1)
				if err != nil {
					ret <- err
					return
				}
				ret <- migr
				go migr.Buffer()
				return

			} else if err != nil {
				ret <- err
				return
			}

			migr, err := m.newMigration(suint(from), int(prev))
			if err != nil {
				ret <- err
				return
			}

			ret <- migr
			go migr.Buffer()
			from = int(prev)
		}
	}
}

func (m *Migrate) readUp(from int, limit int, ret chan<- interface{}) {
	defer close(ret)

	// check if from version exists
	if from >= 0 {
		if m.versionExists(suint(from)) != nil {
			ret <- os.ErrNotExist
			return
		}
	}

	if limit == 0 {
		ret <- ErrNoChange
		return
	}

	count := 0
	for count < limit || limit == -1 {
		if m.stop() {
			return
		}

		// apply first migration if from is nil version
		if from == -1 {
			firstVersion, err := m.sourceDrv.First()
			if err != nil {
				ret <- err
				return
			}

			migr, err := m.newMigration(firstVersion, int(firstVersion))
			if err != nil {
				ret <- err
				return
			}

			ret <- migr
			go migr.Buffer()
			from = int(firstVersion)
			count++
			continue
		}

		// apply next migration
		next, err := m.sourceDrv.Next(suint(from))
		if os.IsNotExist(err) {
			// no limit, but no migrations applied?
			if limit == -1 && count == 0 {
				ret <- ErrNoChange
				return
			}

			// no limit, reached end
			if limit == -1 {
				return
			}

			// reached end, and didn't apply any migrations
			if limit > 0 && count == 0 {
				ret <- os.ErrNotExist
				return
			}

			// applied less migrations than limit?
			if count < limit {
				ret <- ErrShortLimit{suint(limit - count)}
				return
			}
		}
		if err != nil {
			ret <- err
			return
		}

		migr, err := m.newMigration(next, int(next))
		if err != nil {
			ret <- err
			return
		}

		ret <- migr
		go migr.Buffer()
		from = int(next)
		count++
	}
}

func (m *Migrate) readDown(from int, limit int, ret chan<- interface{}) {
	defer close(ret)

	// check if from version exists
	if from >= 0 {
		if m.versionExists(suint(from)) != nil {
			ret <- os.ErrNotExist
			return
		}
	}

	if limit == 0 {
		ret <- ErrNoChange
		return
	}

	// no change if already at nil version
	if from == -1 && limit == -1 {
		ret <- ErrNoChange
		return
	}

	// can't go over limit if already at nil version
	if from == -1 && limit > 0 {
		ret <- os.ErrNotExist
		return
	}

	count := 0
	for count < limit || limit == -1 {
		if m.stop() {
			return
		}

		prev, err := m.sourceDrv.Prev(suint(from))
		if os.IsNotExist(err) {
			// no limit or haven't reached limit, apply "first" migration
			if limit == -1 || limit-count > 0 {
				firstVersion, err := m.sourceDrv.First()
				if err != nil {
					ret <- err
					return
				}

				migr, err := m.newMigration(firstVersion, -1)
				if err != nil {
					ret <- err
					return
				}
				ret <- migr
				go migr.Buffer()
				count++
			}

			if count < limit {
				ret <- ErrShortLimit{suint(limit - count)}
			}
			return
		}
		if err != nil {
			ret <- err
			return
		}

		migr, err := m.newMigration(suint(from), int(prev))
		if err != nil {
			ret <- err
			return
		}

		ret <- migr
		go migr.Buffer()
		from = int(prev)
		count++
	}
}

// ret chan expects *Migration or error
func (m *Migrate) runMigrations(ret <-chan interface{}) error {
	for r := range ret {

		if m.stop() {
			return nil
		}

		switch r.(type) {
		case error:
			return r.(error)

		case *Migration:
			migr := r.(*Migration)

			if migr.Body == nil {
				m.logVerbosePrintf("Execute %v\n", migr.StringLong())
				if err := m.databaseDrv.Run(migr.TargetVersion, nil); err != nil {
					return err
				}

			} else {
				m.logVerbosePrintf("Read and execute %v\n", migr.StringLong())
				if err := m.databaseDrv.Run(migr.TargetVersion, migr.BufferedBody); err != nil {
					return err
				}
			}

			endTime := time.Now()
			readTime := migr.FinishedReading.Sub(migr.StartedBuffering)
			runTime := endTime.Sub(migr.FinishedReading)

			// log either verbose or normal
			if m.Log != nil {
				if m.Log.Verbose() {
					m.logPrintf("Finished %v (read %v, ran %v)\n", migr.StringLong(), readTime, runTime)
				} else {
					m.logPrintf("%v (%v)\n", migr.StringLong(), readTime+runTime)
				}
			}

		default:
			panic("unknown type")
		}
	}
	return nil
}

func (m *Migrate) versionExists(version uint) error {
	// try up migration first
	up, _, err := m.sourceDrv.ReadUp(version)
	if err == nil {
		defer up.Close()
	}
	if os.IsExist(err) {
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}

	// then try down migration
	down, _, err := m.sourceDrv.ReadDown(version)
	if err == nil {
		defer down.Close()
	}
	if os.IsExist(err) {
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}

	return os.ErrNotExist
}

func (m *Migrate) stop() bool {
	if m.isGracefulStop {
		return true
	}

	select {
	case <-m.GracefulStop:
		m.isGracefulStop = true
		return true

	default:
		return false
	}
}

func (m *Migrate) newMigration(version uint, targetVersion int) (*Migration, error) {
	var migr *Migration

	if targetVersion >= int(version) {
		r, identifier, err := m.sourceDrv.ReadUp(version)
		if os.IsNotExist(err) {
			// create "empty" migration
			migr, err = NewMigration(nil, "", version, targetVersion)
			if err != nil {
				return nil, err
			}

		} else if err != nil {
			return nil, err

		} else {
			// create migration from up source
			migr, err = NewMigration(r, identifier, version, targetVersion)
			if err != nil {
				return nil, err
			}
		}

	} else {
		r, identifier, err := m.sourceDrv.ReadDown(version)
		if os.IsNotExist(err) {
			// create "empty" migration
			migr, err = NewMigration(nil, "", version, targetVersion)
			if err != nil {
				return nil, err
			}

		} else if err != nil {
			return nil, err

		} else {
			// create migration from down source
			migr, err = NewMigration(r, identifier, version, targetVersion)
			if err != nil {
				return nil, err
			}
		}
	}

	if m.PrefetchMigrations > 0 && migr.Body != nil {
		m.logVerbosePrintf("Start buffering %v\n", migr.StringLong())
	} else {
		m.logVerbosePrintf("Scheduled %v\n", migr.StringLong())
	}

	return migr, nil
}

func (m *Migrate) lock() error {
	m.isLockedMu.Lock()
	defer m.isLockedMu.Unlock()

	if !m.isLocked {
		if err := m.databaseDrv.Lock(); err != nil {
			return err
		}
		m.isLocked = true
		return nil
	}

	return ErrLocked
}

func (m *Migrate) unlock() error {
	m.isLockedMu.Lock()
	defer m.isLockedMu.Unlock()

	if err := m.databaseDrv.Unlock(); err != nil {
		// can potentially create deadlock when never succeeds
		// TODO: add timeout
		return err
	}

	m.isLocked = false
	return nil
}

func (m *Migrate) unlockErr(prevErr error) error {
	if err := m.unlock(); err != nil {
		return NewMultiError(prevErr, err)
	}
	return prevErr
}
func (m *Migrate) logPrintf(format string, v ...interface{}) {
	if m.Log != nil {
		m.Log.Printf(format, v...)
	}
}

func (m *Migrate) logVerbosePrintf(format string, v ...interface{}) {
	if m.Log != nil && m.Log.Verbose() {
		m.Log.Printf(format, v...)
	}
}
